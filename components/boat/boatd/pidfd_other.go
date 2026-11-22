//go:build !linux

package main

// adoptPidfd is Linux-only (pidfd_open+epoll); non-Linux falls back to backoff polling.
func adoptPidfd(pid int, alive func() bool) (<-chan struct{}, bool) { return nil, false }
