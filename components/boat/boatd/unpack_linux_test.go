package main

import (
	"syscall"
	"testing"
)

func TestUnpackNoNetwork(t *testing.T) {
	if a := unprivileged(0); a.Cloneflags&syscall.CLONE_NEWNET == 0 {
		t.Error("decompressor as root runs in the host's network namespace")
	}
}
