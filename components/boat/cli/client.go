package main

import (
	"bytes"
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

// loadConfig reads the config file; BOAT_URL / BOAT_API_KEY override it.
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
}

// apiError is the server's {"error":{code,message,request_id}} envelope.
type apiError struct {
	Status    int    `json:"status"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

func (e *apiError) Error() string {
	if e.RequestID != "" {
		return fmt.Sprintf("%s (HTTP %d, %s)", e.Message, e.Status, e.RequestID)
	}
	return fmt.Sprintf("%s (HTTP %d)", e.Message, e.Status)
}

func (c *client) request(method, path string, body io.Reader, ctype string) (*http.Response, error) {
	req, err := http.NewRequest(method, strings.TrimRight(c.base, "/")+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("User-Agent", "boat-cli/"+version)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		e := &apiError{Status: resp.StatusCode, Message: http.StatusText(resp.StatusCode)}
		var env struct{ Error *apiError }
		if json.Unmarshal(b, &env) == nil && env.Error != nil {
			e.Code, e.Message, e.RequestID = env.Error.Code, env.Error.Message, env.Error.RequestID
		}
		return nil, e
	}
	return resp, nil
}

func (c *client) call(method, path string, in, out any) error {
	var body io.Reader
	ctype := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ctype = bytes.NewReader(b), "application/json"
	}
	resp, err := c.request(method, path, body, ctype)
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
