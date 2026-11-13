package main

import (
	"os"
	"time"
)

// The memory dump Firecracker writes (3.8 GiB in the e2e perf VM) is left to
// writeback when the snapshot is not fsynced. boat-e2e 43328227157 saw the next
// new-from-snapshot wait 6.2 s in its boot (7.39 s, gate 5 s) behind those dirty
// pages, with quotas on; 0.56-3.19 s in the 15 runs before. Starting writeback
// while the dump is still being written leaves fewer dirty pages behind it.
const writebackEvery = 250 * time.Millisecond

// startWriteback starts writeback of path (once it exists) every writebackEvery
// until stop, which kicks it once more. Best effort: errors only mean no head start.
func startWriteback(path string) (stop func()) {
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(writebackEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				kickFile(path)
				return
			case <-t.C:
				kickFile(path)
			}
		}
	}()
	return func() { close(done); <-finished }
}

func kickFile(path string) {
	f, err := openJail(path, os.O_RDONLY)
	if err != nil {
		return
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Mode().IsRegular() {
		kickWriteback(f)
	}
}
