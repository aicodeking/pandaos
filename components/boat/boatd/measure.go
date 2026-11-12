package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// boatd -measure-disk: the state disk's blk-iocost model, measured once and kept
// in stateDir/iocost-model. ioCost needs it: the kernel's default model for an SSD
// caps the whole disk near 8.5k rand IOPS (PROTOCOL.md "Disk: measured").
// Direct I/O on a NOCOW file in stateDir, so the numbers are the disk's, not the
// page cache's: 1 MiB sequential writes and reads at queue depth 4, 4 KiB random
// reads and writes at queue depth 32, each for measureFor.

const (
	ioCostModelFile = "iocost-model"
	probeFile       = ".iocost-probe"
	measureCmd      = "boatd -measure-disk"
)

type diskMeasure struct {
	size    int64         // probe file size
	runFor  time.Duration // per phase
	seqQD   int
	randQD  int
	seqBS   int64
	randBS  int64
	openDir func(path string) (*os.File, error) // O_DIRECT where the OS has it
}

func defaultMeasure() diskMeasure {
	return diskMeasure{size: 1 << 30, runFor: 5 * time.Second, seqQD: 4, randQD: 32, seqBS: 1 << 20, randBS: 4 << 10, openDir: openDirect}
}

// run returns the model line: rbps rseqiops rrandiops wbps wseqiops wrandiops.
// Sequential 4 KiB IOPS are not measured separately: they are set to the random
// ones, which is what the disk does at worst (iocost then charges sequential
// I/O no less than random).
func (d diskMeasure) run(dir string) (string, error) {
	path := filepath.Join(dir, probeFile)
	createNoCow(path) // best effort: CoW only adds btrfs work to writes
	defer os.Remove(path)
	f, err := d.openDir(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	wbps, err := d.phase(f, d.seqQD, d.seqBS, true, false)
	if err != nil {
		return "", fmt.Errorf("sequential write: %w", err)
	}
	rbps, err := d.phase(f, d.seqQD, d.seqBS, false, false)
	if err != nil {
		return "", fmt.Errorf("sequential read: %w", err)
	}
	riops, err := d.phase(f, d.randQD, d.randBS, false, true)
	if err != nil {
		return "", fmt.Errorf("random read: %w", err)
	}
	wiops, err := d.phase(f, d.randQD, d.randBS, true, true)
	if err != nil {
		return "", fmt.Errorf("random write: %w", err)
	}
	r, w := int64(riops/float64(d.randBS)), int64(wiops/float64(d.randBS))
	return fmt.Sprintf("rbps=%d rseqiops=%d rrandiops=%d wbps=%d wseqiops=%d wrandiops=%d",
		int64(rbps), r, r, int64(wbps), w, w), nil
}

// phase runs qd workers for runFor and returns bytes/s. Writes end with an fdatasync,
// counted in the time: a disk that only buffers must not look fast.
func (d diskMeasure) phase(f *os.File, qd int, bs int64, write, random bool) (float64, error) {
	blocks := d.size / bs
	if blocks < 1 {
		return 0, errors.New("probe smaller than one block")
	}
	var next, done atomic.Int64
	var firstErr error
	var mu sync.Mutex
	stop := make(chan struct{})
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < qd; w++ {
		buf, err := alignedBuf(int(bs))
		if err != nil {
			return 0, err
		}
		for i := range buf {
			buf[i] = byte(rand.IntN(256)) // incompressible
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer freeBuf(buf)
			for {
				select {
				case <-stop:
					return
				default:
				}
				var blk int64
				if random {
					blk = rand.Int64N(blocks)
				} else {
					blk = (next.Add(1) - 1) % blocks
				}
				var err error
				if write {
					_, err = f.WriteAt(buf, blk*bs)
				} else {
					_, err = f.ReadAt(buf, blk*bs)
				}
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					return
				}
				done.Add(bs)
			}
		}()
	}
	time.Sleep(d.runFor)
	close(stop)
	wg.Wait()
	if firstErr != nil {
		return 0, firstErr
	}
	if write {
		if err := f.Sync(); err != nil {
			return 0, err
		}
	}
	return float64(done.Load()) / time.Since(start).Seconds(), nil
}

// measureDisk: boatd -measure-disk. Writes stateDir/iocost-model and prints it.
func measureDisk(c Config) error {
	if err := os.MkdirAll(c.StateDir, 0o755); err != nil {
		return err
	}
	log.Printf("measure-disk: %s, about 25 s of direct I/O on a 1 GiB file", c.StateDir)
	model, err := defaultMeasure().run(c.StateDir)
	if err != nil {
		return err
	}
	if !reIOCostModel.MatchString(model) {
		return fmt.Errorf("measured model %q is malformed", model)
	}
	path := filepath.Join(c.StateDir, ioCostModelFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(model+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	fmt.Printf("%s\n", model)
	log.Printf("measure-disk: wrote %s; boatd uses it while ioCostModel is unset", path)
	return nil
}

// storedIOCostModel: stateDir/iocost-model, "" when absent or malformed.
func storedIOCostModel(stateDir string) string {
	b, err := os.ReadFile(filepath.Join(stateDir, ioCostModelFile))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("%s: %v", ioCostModelFile, err)
		}
		return ""
	}
	m := strings.TrimSpace(string(b))
	if !reIOCostModel.MatchString(m) {
		log.Printf("%s: malformed, ignored (%q); rerun %s", ioCostModelFile, m, measureCmd)
		return ""
	}
	return m
}

// alignedBuf: direct I/O wants page-aligned buffers; an anonymous mapping is one.
func alignedBuf(n int) ([]byte, error) {
	return syscall.Mmap(-1, 0, n, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
}

func freeBuf(b []byte) { syscall.Munmap(b) }
