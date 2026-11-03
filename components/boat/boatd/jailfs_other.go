//go:build !linux

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
)

// Dev and tests only: boatd runs VMs on Linux.

func ficlone(dst, src *os.File) error { return errors.ErrUnsupported }

func linkFile(f *os.File, dst string) error { return os.Link(f.Name(), dst) }

func dialJail(ctx context.Context, path string) (net.Conn, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if st.Mode()&os.ModeSocket == 0 {
		return nil, fmt.Errorf("%s: not a socket", path)
	}
	var d net.Dialer
	return d.DialContext(ctx, "unix", path)
}

func enableSimpleQuota(string) error { return errors.ErrUnsupported }
