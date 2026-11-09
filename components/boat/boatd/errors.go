package main

import (
	"errors"
	"fmt"
	"net/http"
)

// apiErr is an error with its HTTP status, see writeErr.
type apiErr struct {
	code int
	msg  string
	kind string // "code" in the error body, when set (see PROTOCOL.md)
}

func (e *apiErr) Error() string { return e.msg }

func errf(code int, f string, a ...any) error { return &apiErr{code: code, msg: fmt.Sprintf(f, a...)} }

// Error codes. Host admission refusals are host_capacity; a request with fail_fast
// gets them (and the startup image wait) as no_ready_machine.
const (
	codeHostCapacity   = "host_capacity"
	codeNoReadyMachine = "no_ready_machine"
)

// errCapacity: the host cannot take this VM now (RAM, vCPUs, real free memory, addresses).
func errCapacity(f string, a ...any) error {
	return &apiErr{code: http.StatusServiceUnavailable, msg: fmt.Sprintf(f, a...), kind: codeHostCapacity}
}

// failFast relabels an admission refusal for a fail_fast request.
func failFast(err error) error {
	var ae *apiErr
	if errors.As(err, &ae) && ae.kind == codeHostCapacity {
		return &apiErr{code: ae.code, msg: ae.msg, kind: codeNoReadyMachine}
	}
	return err
}

var errNotFound = &apiErr{code: http.StatusNotFound, msg: "vm not found"}
