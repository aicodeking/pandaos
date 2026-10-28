package main

// Placeholder PandaConnect wire protocol. Everything HTTP/JSON-specific lives
// in this file so it can be swapped for the real protocol later.
//
// Box-facing endpoints: POST /v1/pair/start, GET /v1/pair/status,
// POST /v1/heartbeat (response {"cancel":[ids]}), GET /v1/jobs/next,
// POST /v1/jobs/{id}/result. Controllers cancel a job with
// POST /v1/jobs/{id}/cancel; the box learns about it from the next heartbeat.

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Caps struct {
	Hostname   string `json:"hostname"`
	CPUs       int    `json:"cpus"`
	MemTotalMB int    `json:"memTotalMB"`
	MemAvailMB int    `json:"memAvailMB"`
	Slots      int    `json:"slots"`
	FreeSlots  int    `json:"freeSlots"`
	Version    string `json:"version"`
}

type Job struct {
	ID          string   `json:"id"`
	Argv        []string `json:"argv"`
	TimeoutSec  int      `json:"timeoutSec"`
	MemoryMaxMB int      `json:"memoryMaxMB"`
	KeepWorkdir bool     `json:"keepWorkdir"`
}

type Result struct {
	ExitCode int    `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	// Cancelled is set when the job was stopped by a cancel or daemon shutdown.
	Cancelled bool `json:"cancelled,omitempty"`
	// Interrupted is set for a job whose agentd died mid-run (reported at next start).
	Interrupted bool `json:"interrupted,omitempty"`
}

var (
	errUnauthorized = errors.New("relay rejected token")
	errPairExpired  = errors.New("relay does not know pairing code")
)

// Long-polls hold for up to 30s; leave headroom above that. Redirects are not
// followed so a relay cannot bounce the bearer token to another URL or scheme.
var httpClient = &http.Client{
	Timeout:       60 * time.Second,
	Transport:     tlsTransport(),
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func tlsTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return t
}

type client struct {
	base, token string
}

func newClient(base, token string) *client {
	return &client{base: strings.TrimRight(base, "/"), token: token}
}

func (c *client) do(ctx context.Context, method, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return 0, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	defer io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)) // allow connection reuse
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return resp.StatusCode, errUnauthorized
	case resp.StatusCode >= 300:
		return resp.StatusCode, fmt.Errorf("%s %s: %s", method, path, resp.Status)
	case out != nil && resp.StatusCode != http.StatusNoContent:
		return resp.StatusCode, json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
	}
	return resp.StatusCode, nil
}

func (c *client) pairStart(ctx context.Context, code string, caps Caps) error {
	in := struct {
		Code     string `json:"code"`
		Hostname string `json:"hostname"`
		Caps     Caps   `json:"caps"`
	}{code, caps.Hostname, caps}
	_, err := c.do(ctx, http.MethodPost, "/v1/pair/start", in, nil)
	return err
}

// pairStatus returns "" while the code is still pending approval.
func (c *client) pairStatus(ctx context.Context, code string) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	status, err := c.do(ctx, http.MethodGet, "/v1/pair/status?code="+url.QueryEscape(code), nil, &out)
	if status == http.StatusNotFound {
		return "", errPairExpired
	}
	return out.Token, err
}

// heartbeat reports caps and returns the ids of jobs the relay wants cancelled.
func (c *client) heartbeat(ctx context.Context, caps Caps) ([]string, error) {
	var out struct {
		Cancel []string `json:"cancel"`
	}
	_, err := c.do(ctx, http.MethodPost, "/v1/heartbeat", caps, &out)
	return out.Cancel, err
}

// nextJob returns nil, nil when the long-poll ends without a job.
func (c *client) nextJob(ctx context.Context) (*Job, error) {
	var j Job
	status, err := c.do(ctx, http.MethodGet, "/v1/jobs/next", nil, &j)
	if err != nil || status == http.StatusNoContent {
		return nil, err
	}
	if j.ID == "" {
		return nil, errors.New("relay sent job without id")
	}
	return &j, nil
}

func (c *client) postResult(ctx context.Context, id string, r Result) error {
	_, err := c.do(ctx, http.MethodPost, "/v1/jobs/"+url.PathEscape(id)+"/result", r, nil)
	return err
}
