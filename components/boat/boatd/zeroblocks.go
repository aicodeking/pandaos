package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"syscall"
)

// ---- zero blocks out of a first memory dump ----
//
// A first dump (no base: after a cold boot, or a Full snapshot) holds every page
// the guest dirtied since boot, and Firecracker 1.17 writes them all, also those
// the guest has freed since and the balloon reported (free page reporting): the
// host dropped them, so they read back as zeros, but KVM's dirty log still has
// them. They were most of a first stop's write (about 2.4 GiB of an 8 GiB VM's
// dump). Without a base a hole IS zeros, so punching all-zero blocks out of
// the dump before it is flushed changes nothing a restore reads, and their
// dirty page cache is dropped instead of written. Never on a diff with a base:
// there a hole means "the base's page".
//
// Only whole, aligned 2 MiB blocks: free page reporting hands back
// pageblock-sized (2 MiB on x86) runs, and smaller holes would split the file
// into extents that every later reflink of it pays for.

const zeroBlock = 2 << 20

// punchZeroBlocks scans f's data regions in aligned zeroBlock steps and punches
// each all-zero block. It returns the bytes scanned and punched.
func punchZeroBlocks(f *os.File) (scanned, punched int64, err error) {
	st, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	size := st.Size()
	buf := make([]byte, zeroBlock)
	zero := make([]byte, zeroBlock)
	for off := int64(0); off < size; {
		data, err := f.Seek(off, seekData)
		if errors.Is(err, syscall.ENXIO) {
			break
		}
		if err != nil {
			data = off // no SEEK_DATA: scan the rest
		}
		hole, err := f.Seek(data, seekHole)
		if err != nil {
			hole = size
		}
		for b := data / zeroBlock * zeroBlock; b < hole; b += zeroBlock {
			n := min(int64(zeroBlock), size-b)
			if _, err := f.ReadAt(buf[:n], b); err != nil && !errors.Is(err, io.EOF) {
				return scanned, punched, err
			}
			scanned += n
			if n == zeroBlock && bytes.Equal(buf, zero) {
				if err := punchHole(f, b, n); err != nil {
					return scanned, punched, err
				}
				punched += n
			}
		}
		off = max(hole, (hole+zeroBlock-1)/zeroBlock*zeroBlock)
	}
	return scanned, punched, nil
}
