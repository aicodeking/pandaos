//go:build !linux

package main

// syncFS is only needed on the Linux host filesystem used in production.
func syncFS(path string) error { return nil }
