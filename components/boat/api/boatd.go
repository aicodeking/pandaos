package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// boatdClient talks to boatd's internal API, over a unix socket by default.
// addr is a socket path ("/run/boat/boatd.sock", optionally "unix:" prefixed)
// or an http:// URL (tests, dev).
type boatdClient struct {
	base string
	hc   *http.Client
}

func newBoatd(addr string) *boatdClient {
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return &boatdClient{base: strings.TrimRight(addr, "/"), hc: &http.Client{}}
	}
	sock := strings.TrimPrefix(addr, "unix:")
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}
	return &boatdClient{base: "http://boatd", hc: &http.Client{Transport: tr}}
}

func (c *boatdClient) do(ctx context.Context, method, path string, body io.Reader, ctype string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	return c.hc.Do(req)
}

// call sends in as JSON (nil = no body) and decodes a 2xx response into out.
// Non-2xx returns a *boatdError carrying status and body.
func (c *boatdClient) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	ctype := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ctype = bytes.NewReader(b), "application/json"
	}
	resp, err := c.do(ctx, method, path, body, ctype)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return &boatdError{Status: resp.StatusCode, Body: b}
	}
	if out == nil || len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	return json.Unmarshal(b, out)
}

type boatdError struct {
	Status int
	Body   []byte
}

func (e *boatdError) Error() string {
	return fmt.Sprintf("boatd: HTTP %d: %s", e.Status, bytes.TrimSpace(e.Body))
}

// code is boatd's machine-readable "code" next to "error" (PROTOCOL.md), or "".
func (e *boatdError) code() string {
	var v struct {
		Code string `json:"code"`
	}
	json.Unmarshal(e.Body, &v)
	return v.Code
}

// message extracts boatd's {"error": "..."} or {"error":{"message": "..."}}.
func (e *boatdError) message() string {
	var v struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(e.Body, &v) == nil && len(v.Error) > 0 {
		var s string
		if json.Unmarshal(v.Error, &s) == nil {
			return s
		}
		var o struct{ Message string }
		if json.Unmarshal(v.Error, &o) == nil && o.Message != "" {
			return o.Message
		}
	}
	return http.StatusText(e.Status)
}
