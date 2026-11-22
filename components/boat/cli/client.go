package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	URL string `json:"url"`
	Key string `json:"key"`
}

func configPath(getenv func(string) string) string {
	if p := getenv("BOAT_CONFIG"); p != "" {
		return p
	}
	if x := getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "boat", "config.json")
	}
	return filepath.Join(homeDir(getenv), ".config", "boat", "config.json")
}

// homeDir also covers Windows, which has USERPROFILE instead of HOME.
func homeDir(getenv func(string) string) string {
	if h := getenv("HOME"); h != "" {
		return h
	}
	return getenv("USERPROFILE")
}

// apiOrigin accepts boat.dev's API base (https://host/api/v1) for this CLI,
// which talks to the native /v1 next to it.
func apiOrigin(u string) string {
	u = strings.TrimRight(strings.TrimSpace(u), "/")
	for _, suf := range []string{"/api/v1", "/v1"} {
		if t, ok := strings.CutSuffix(u, suf); ok {
			return strings.TrimRight(t, "/")
		}
	}
	return u
}

// loadCfg is loadConfig plus the global --api-url, which always wins.
func (a *app) loadCfg() (Config, error) {
	cfg, err := loadConfig(a.getenv)
	if a.apiURL != "" {
		cfg.URL = apiOrigin(a.apiURL)
	}
	return cfg, err
}

// loadConfig reads the config file; BOAT_URL (or boat.dev's BOAT_API_URL) and
// BOAT_API_KEY override it.
func loadConfig(getenv func(string) string) (Config, error) {
	var c Config
	b, err := os.ReadFile(configPath(getenv))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return c, err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &c); err != nil {
			return c, fmt.Errorf("%s: %w", configPath(getenv), err)
		}
	}
	if v := getenv("BOAT_URL"); v != "" {
		c.URL = v
	}
	c.URL = apiOrigin(c.URL)
	if v := getenv("BOAT_API_URL"); v != "" { // boat.dev's name (env of --api-url)
		c.URL = apiOrigin(v)
	}
	if v := getenv("BOAT_API_KEY"); v != "" {
		c.Key = v
	}
	return c, nil
}

func saveConfig(getenv func(string) string, c Config) error {
	p := configPath(getenv)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

type client struct {
	base string
	key  string
	hc   *http.Client
	// alias expands VM refs like "current" before they are looked up.
	alias func(string) (string, error)
}

// apiError is the server's {"error":{code,message,request_id}} envelope.
type apiError struct {
	Status         int    `json:"status"`
	Code           string `json:"code"`
	Message        string `json:"message"`
	RequestID      string `json:"request_id,omitempty"`
	RequestIDCamel string `json:"requestId,omitempty"`
	// VM is the error body's top-level "vm" object (e.g. the stopped child of
	// a failed no_env fork), when the server sent one.
	VM map[string]any `json:"-"`
	// VMs is the top-level "vms" array (a failed batch create names every VM
	// that exists, stopped or still running).
	VMs []map[string]any `json:"-"`
}

// UnmarshalJSON accepts request_id and the compat envelope's requestId.
func (e *apiError) UnmarshalJSON(b []byte) error {
	type plain apiError
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	if p.RequestID == "" {
		p.RequestID = p.RequestIDCamel
	}
	p.RequestIDCamel = ""
	*e = apiError(p)
	return nil
}

func (e *apiError) Error() string {
	if e.RequestID != "" {
		return fmt.Sprintf("%s (HTTP %d, %s)", e.Message, e.Status, e.RequestID)
	}
	return fmt.Sprintf("%s (HTTP %d)", e.Message, e.Status)
}

func (c *client) request(method, path string, body io.Reader, ctype string) (*http.Response, error) {
	return c.requestCtx(context.Background(), method, path, body, ctype)
}

func (c *client) requestCtx(ctx context.Context, method, path string, body io.Reader, ctype string) (*http.Response, error) {
	return c.requestCtxHeaders(ctx, method, path, body, ctype, nil)
}

func (c *client) requestCtxHeaders(ctx context.Context, method, path string, body io.Reader, ctype string, headers map[string]string) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.base, "/")+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("User-Agent", "boat-cli/"+version)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		e := &apiError{Status: resp.StatusCode, Message: http.StatusText(resp.StatusCode)}
		// error is either an object or a bare string; decode it on its own so a
		// string never aborts reading the top-level request id.
		var env struct {
			Error      json.RawMessage `json:"error"`
			RequestID  string          `json:"requestId"`
			RequestID2 string          `json:"request_id"`
			VM         json.RawMessage `json:"vm"`
			VMs        json.RawMessage `json:"vms"`
		}
		if json.Unmarshal(b, &env) == nil && len(env.Error) > 0 {
			var obj apiError
			var msg string
			if json.Unmarshal(env.Error, &obj) == nil {
				e.Code, e.RequestID = obj.Code, obj.RequestID
				if obj.Message != "" {
					e.Message = obj.Message
				}
			} else if json.Unmarshal(env.Error, &msg) == nil && msg != "" {
				e.Message = msg
			}
		}
		if len(env.VM) > 0 {
			var vm map[string]any
			if json.Unmarshal(env.VM, &vm) == nil {
				e.VM = vm
			}
		}
		if len(env.VMs) > 0 {
			var vms []map[string]any
			if json.Unmarshal(env.VMs, &vms) == nil {
				e.VMs = vms
			}
		}
		if e.RequestID == "" {
			e.RequestID = env.RequestID
		}
		if e.RequestID == "" {
			e.RequestID = env.RequestID2
		}
		if e.RequestID == "" {
			e.RequestID = resp.Header.Get("X-Request-Id")
		}
		return nil, e
	}
	return resp, nil
}

func (c *client) call(method, path string, in, out any) error {
	return c.callH(method, path, in, out, nil)
}

func (c *client) callH(method, path string, in, out any, headers map[string]string) error {
	var body io.Reader
	ctype := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ctype = bytes.NewReader(b), "application/json"
	}
	resp, err := c.requestCtxHeaders(context.Background(), method, path, body, ctype, headers)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type obj = map[string]any

func s(m obj, k string) string {
	switch v := m[k].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func vmPath(id string, rest ...string) string {
	p := "/v1/vms/" + url.PathEscape(id)
	for _, r := range rest {
		p += "/" + r
	}
	return p
}

// resolveVM accepts an id or a unique name.
func (c *client) resolveVM(ref string) (obj, error) {
	if c.alias != nil {
		var err error
		if ref, err = c.alias(ref); err != nil {
			return nil, err
		}
	}
	var vm obj
	err := c.call("GET", vmPath(ref), nil, &vm)
	var ae *apiError
	if err == nil || !errors.As(err, &ae) || ae.Status != 404 {
		return vm, err
	}
	var list struct{ VMs []obj }
	if err := c.call("GET", "/v1/vms", nil, &list); err != nil {
		return nil, err
	}
	var hits []obj
	for _, v := range list.VMs {
		if s(v, "name") == ref {
			hits = append(hits, v)
		}
	}
	switch len(hits) {
	case 0:
		return nil, fmt.Errorf("no VM with id or name %q", ref)
	case 1:
		return hits[0], nil
	}
	return nil, fmt.Errorf("name %q is ambiguous (%d VMs); use the id", ref, len(hits))
}
