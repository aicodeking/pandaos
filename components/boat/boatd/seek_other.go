//go:build !linux

package main

import (
	"errors"
	"os"
)

const seekData, seekHole = 4, 3 // darwin/BSD values

func setMemoryMerge() error { return errors.ErrUnsupported }

func punchHole(*os.File, int64, int64) error { return errors.ErrUnsupported }
