package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPunchZeroBlocks(t *testing.T) {
	p := filepath.Join(t.TempDir(), "dump")
	want := make([]byte, 4*zeroBlock+4096) // zero, data, zero, one data byte, a short zero tail
	for i := zeroBlock; i < 2*zeroBlock; i++ {
		want[i] = byte(i)
	}
	want[3*zeroBlock+5] = 1
	if err := os.WriteFile(p, want, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	scanned, punched, err := punchZeroBlocks(f)
	if errors.Is(err, errors.ErrUnsupported) {
		t.Skip("no hole punching on this OS")
	}
	if err != nil {
		t.Fatal(err)
	}
	if scanned != int64(len(want)) || punched != 2*zeroBlock {
		t.Fatalf("scanned %d punched %d", scanned, punched)
	}
	got, _ := os.ReadFile(p)
	if !bytes.Equal(got, want) {
		t.Fatal("punching changed the content")
	}
	if a := allocatedBytes(p); a > 2*zeroBlock+2*4096 {
		t.Errorf("still %d bytes allocated", a)
	}
}
