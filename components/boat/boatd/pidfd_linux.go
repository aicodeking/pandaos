//go:build linux

package main

import (
	"errors"
	"sync"
	"syscall"
	"time"
)

// sysPidfdOpen is Linux's pidfd_open(2) syscall number (unified 434 across
// amd64, arm64 and other modern architectures since Linux 5.3).
const sysPidfdOpen = 434

type pidfdWatcher struct {
	once    sync.Once
	epfd    int
	initErr error

	mu      sync.Mutex
	waiters map[int]chan struct{}
}

var pidfdMon pidfdWatcher

func (w *pidfdWatcher) init() {
	epfd, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		w.initErr = err
		return
	}
	w.epfd = epfd
	w.waiters = map[int]chan struct{}{}
	go w.loop()
}

func (w *pidfdWatcher) watch(pidfd int) (<-chan struct{}, bool) {
	w.once.Do(w.init)
	if w.initErr != nil {
		return nil, false
	}
	done := make(chan struct{})
	w.mu.Lock()
	w.waiters[pidfd] = done
	ev := syscall.EpollEvent{Events: syscall.EPOLLIN, Fd: int32(pidfd)}
	err := syscall.EpollCtl(w.epfd, syscall.EPOLL_CTL_ADD, pidfd, &ev)
	if err != nil {
		delete(w.waiters, pidfd)
		w.mu.Unlock()
		return nil, false
	}
	w.mu.Unlock()
	return done, true
}

func (w *pidfdWatcher) loop() {
	var events [64]syscall.EpollEvent
	for {
		n, err := syscall.EpollWait(w.epfd, events[:], -1)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		for i := 0; i < n; i++ {
			fd := int(events[i].Fd)
			w.mu.Lock()
			ch, ok := w.waiters[fd]
			if ok {
				delete(w.waiters, fd)
			}
			w.mu.Unlock()
			if ok {
				_ = syscall.EpollCtl(w.epfd, syscall.EPOLL_CTL_DEL, fd, nil)
				_ = syscall.Close(fd)
				close(ch)
			}
		}
	}
}

// adoptPidfd watches pid for exit via pidfd_open(2)+epoll so process exit is
// observed immediately without polling. It verifies alive() after opening the
// pidfd to guard against PID reuse. Returns (nil, false) when pidfd is
// unavailable so callers can fall back to backoff polling.
func adoptPidfd(pid int, alive func() bool) (<-chan struct{}, bool) {
	if pid <= 0 {
		return nil, false
	}
	fd, _, errno := syscall.Syscall(sysPidfdOpen, uintptr(pid), 0, 0)
	if errno != 0 {
		if errno == syscall.ESRCH && !alive() {
			done := make(chan struct{})
			close(done)
			return done, true
		}
		return nil, false
	}
	pidfd := int(fd)
	if !alive() {
		_ = syscall.Close(pidfd)
		done := make(chan struct{})
		close(done)
		return done, true
	}
	done, ok := pidfdMon.watch(pidfd)
	if !ok {
		_ = syscall.Close(pidfd)
		return nil, false
	}
	return done, true
}
