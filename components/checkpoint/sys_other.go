//go:build !linux

package main

// btrfs is Linux-only; elsewhere (dev machines) nothing is a subvolume.
func isSubvol(string) bool { return false }

func swap(a, b string) error { return swapRenames(a, b) }
