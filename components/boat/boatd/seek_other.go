//go:build !linux

package main

import "errors"

const seekData, seekHole = 4, 3 // darwin/BSD values

func setMemoryMerge() error { return errors.ErrUnsupported }
