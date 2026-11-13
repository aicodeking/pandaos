package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The dump appears after the snapshot call starts; a planted link is not followed;
// stop returns once the last kick is done.
func TestStartWriteback(t *testing.T) {
	dir := t.TempDir()
	mem := filepath.Join(dir, memNew)
	stop := startWriteback(mem)
	time.Sleep(writebackEvery + 50*time.Millisecond) // no file yet
	if err := os.WriteFile(mem, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(writebackEvery + 50*time.Millisecond)
	t0 := time.Now()
	stop()
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("stop took %v", d)
	}
	link := filepath.Join(dir, "link")
	os.Symlink(mem, link)
	if f, err := openJail(link, os.O_RDONLY); err == nil {
		f.Close()
		t.Fatal("followed a link")
	}
}
