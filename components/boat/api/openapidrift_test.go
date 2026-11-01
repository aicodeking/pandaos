package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

// The drift check. Every request the whole test suite makes goes through
// recorder. Each is validated against openapi.json:
//
//   - the JSON request body against the operation's request schema,
//   - the JSON response against the schema for its status (unknown properties
//     fail unless the schema says additionalProperties),
//   - the status must be declared (or fall to the operation's default),
//   - the Go struct a handler decodes into must have exactly the properties
//     the spec lists for its request body.
//
// After the suite, a full run also fails for any operation that never returned
// a 2xx, so an undocumented or untested route cannot hide.

type openapiDoc struct {
	Paths      map[string]map[string]map[string]any `json:"paths"`
	Components struct {
		Schemas   map[string]any `json:"schemas"`
		Responses map[string]any `json:"responses"`
	} `json:"components"`
}

var (
	driftMu    sync.Mutex
	driftSpec  openapiDoc
	driftErrs  = map[string]bool{}
	driftSeen  = map[string]bool{} // pattern -> saw 2xx
	driftBody  = map[string]map[string]bool{}
	driftSetup sync.Once
)

func driftErr(format string, a ...any) {
	driftMu.Lock()
	defer driftMu.Unlock()
	driftErrs[fmt.Sprintf(format, a...)] = true
}

func loadDriftSpec() {
	driftSetup.Do(func() {
		if err := json.Unmarshal(openapiSpec, &driftSpec); err != nil {
			panic(err)
		}
		decodeHook = func(r *http.Request, v any) {
			p := info(r).pattern
			fields := map[string]bool{}
			t := reflect.TypeOf(v).Elem()
			for i := 0; i < t.NumField(); i++ {
				name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
				if name != "" && name != "-" {
					fields[name] = true
				}
			}
			driftMu.Lock()
			driftBody[p] = fields
			driftMu.Unlock()
		}
	})
}

// operation finds the spec entry for "METHOD /path".
func operation(pattern string) (map[string]any, string) {
	method, path, _ := strings.Cut(pattern, " ")
	return driftSpec.Paths[path][strings.ToLower(method)], strings.ToLower(method)
}

func resolveRef(s map[string]any) map[string]any {
	for {
		ref, ok := s["$ref"].(string)
		if !ok {
			return s
		}
		name := ref[strings.LastIndex(ref, "/")+1:]
		if strings.Contains(ref, "/responses/") {
			next, _ := driftSpec.Components.Responses[name].(map[string]any)
			return next
		}
		next, _ := driftSpec.Components.Schemas[name].(map[string]any)
		s = next
	}
}

func typeOK(v any, typ string) bool {
	switch typ {
	case "string":
		_, ok := v.(string)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == float64(int64(f))
	case "number":
		_, ok := v.(float64)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "null":
		return v == nil
	}
	return false
}

// validate checks v against a schema and returns problems, with paths.
func validate(v any, s map[string]any, at string) []string {
	s = resolveRef(s)
	if s == nil {
		return []string{at + ": unresolved schema"}
	}
	var out []string
	if all, ok := s["allOf"].([]any); ok {
		merged := map[string]any{"type": "object", "properties": map[string]any{}}
		req := []any{}
		for _, x := range all {
			sub := resolveRef(x.(map[string]any))
			for k, p := range asMap(sub["properties"]) {
				merged["properties"].(map[string]any)[k] = p
			}
			if r, ok := sub["required"].([]any); ok {
				req = append(req, r...)
			}
			if sub["additionalProperties"] != nil {
				merged["additionalProperties"] = sub["additionalProperties"]
			}
		}
		merged["required"] = req
		return validate(v, merged, at)
	}
	if enum, ok := s["enum"].([]any); ok {
		for _, e := range enum {
			if reflect.DeepEqual(e, v) {
				return nil
			}
		}
		return []string{fmt.Sprintf("%s: %v not in enum %v", at, v, enum)}
	}
	switch t := s["type"].(type) {
	case string:
		if !typeOK(v, t) {
			return []string{fmt.Sprintf("%s: want %s, got %T", at, t, v)}
		}
	case []any:
		ok := false
		for _, x := range t {
			ok = ok || typeOK(v, x.(string))
		}
		if !ok {
			return []string{fmt.Sprintf("%s: want %v, got %T", at, t, v)}
		}
	}
	if m, ok := v.(map[string]any); ok {
		props := asMap(s["properties"])
		if r, ok := s["required"].([]any); ok {
			for _, x := range r {
				if _, has := m[x.(string)]; !has {
					out = append(out, fmt.Sprintf("%s: missing required %s", at, x))
				}
			}
		}
		_, addIsSchema := s["additionalProperties"].(map[string]any)
		openObj := s["additionalProperties"] == true || addIsSchema || s["properties"] == nil
		for k, val := range m {
			if ps, ok := props[k].(map[string]any); ok {
				out = append(out, validate(val, ps, at+"."+k)...)
			} else if addIsSchema {
				out = append(out, validate(val, s["additionalProperties"].(map[string]any), at+"."+k)...)
			} else if !openObj {
				out = append(out, fmt.Sprintf("%s: property %q is not in the spec", at, k))
			}
		}
	}
	if a, ok := v.([]any); ok {
		if items, ok := s["items"].(map[string]any); ok {
			for i, x := range a {
				out = append(out, validate(x, items, fmt.Sprintf("%s[%d]", at, i))...)
			}
		}
	}
	return out
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

type capture struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (c *capture) WriteHeader(s int) {
	if c.status == 0 {
		c.status = s
	}
	c.ResponseWriter.WriteHeader(s)
}

func (c *capture) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = 200
	}
	if c.body.Len() < 4<<20 {
		c.body.Write(b)
	}
	return c.ResponseWriter.Write(b)
}

func (c *capture) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// recorder wraps the server for every test (see setup).
func recorder(s *server) http.Handler {
	loadDriftSpec()
	var patterns sync.Map // *http.Request -> matched route
	s.observe = func(p string, r *http.Request, _ int) { patterns.Store(r, p) }
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqBody []byte
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") || r.Header.Get("Content-Type") == "" {
			reqBody, _ = io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(reqBody))
		}
		c := &capture{ResponseWriter: w}
		s.ServeHTTP(c, r)
		p, _ := patterns.LoadAndDelete(r)
		pattern, _ := p.(string)
		checkExchange(pattern, reqBody, c)
	})
}

func checkExchange(pattern string, reqBody []byte, c *capture) {
	if pattern == "" {
		return
	}
	op, _ := operation(pattern)
	if op == nil {
		driftErr("%s: no such operation in openapi.json", pattern)
		return
	}
	if c.status/100 == 2 {
		driftMu.Lock()
		driftSeen[pattern] = true
		driftMu.Unlock()
	}
	if len(bytes.TrimSpace(reqBody)) > 0 && c.status/100 == 2 {
		rb := asMap(asMap(asMap(op["requestBody"])["content"])["application/json"])
		if schema := asMap(rb["schema"]); schema != nil {
			var v any
			if json.Unmarshal(reqBody, &v) == nil {
				for _, p := range validate(v, schema, "request") {
					driftErr("%s: %s", pattern, p)
				}
			}
		} else if asMap(op["requestBody"]) == nil {
			driftErr("%s: got a JSON body but the spec declares none", pattern)
		}
	}
	responses := asMap(op["responses"])
	resp := asMap(responses[fmt.Sprint(c.status)])
	if resp == nil {
		resp = asMap(responses["default"])
	}
	if resp == nil {
		driftErr("%s: status %d is not declared", pattern, c.status)
		return
	}
	resp = resolveRef(resp)
	if c.status/100 == 2 && asMap(responses[fmt.Sprint(c.status)]) == nil {
		driftErr("%s: success status %d is not declared", pattern, c.status)
	}
	content := asMap(resp["content"])
	schema := asMap(asMap(content["application/json"])["schema"])
	ndjson := asMap(asMap(content["application/x-ndjson"])["schema"])
	ct := c.Header().Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "application/json") && schema != nil:
		var v any
		if err := json.Unmarshal(c.body.Bytes(), &v); err != nil {
			driftErr("%s: response is not JSON: %v", pattern, err)
			return
		}
		for _, p := range validate(v, schema, "response") {
			driftErr("%s [%d]: %s", pattern, c.status, p)
		}
	case strings.HasPrefix(ct, "application/json") && len(content) > 0 && c.status/100 == 2:
		driftErr("%s [%d]: returned JSON but the spec has no JSON schema", pattern, c.status)
	case strings.HasPrefix(ct, "application/x-ndjson") && ndjson != nil:
		for _, line := range bytes.Split(c.body.Bytes(), []byte("\n")) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var v any
			if json.Unmarshal(line, &v) == nil {
				for _, p := range validate(v, ndjson, "line") {
					driftErr("%s [%d]: %s", pattern, c.status, p)
				}
			}
		}
	case strings.HasPrefix(ct, "application/x-ndjson") && c.status/100 == 2:
		// ?follow=1 on a route documented as JSON: its line shape is documented there
		if asMap(content["application/x-ndjson"]) == nil {
			driftErr("%s [%d]: returned NDJSON but the spec does not say so", pattern, c.status)
		}
	}
}

// checkBodies compares each decoded request struct with the spec's properties.
func checkBodies() {
	for pattern, fields := range driftBody {
		op, _ := operation(pattern)
		if op == nil {
			continue
		}
		schema := resolveRef(asMap(asMap(asMap(asMap(op["requestBody"])["content"])["application/json"])["schema"]))
		props := asMap(schema["properties"])
		if schema == nil && len(fields) > 0 {
			driftErr("%s: handler accepts %v but the spec declares no JSON body", pattern, keys(fields))
			continue
		}
		for f := range fields {
			if props[f] == nil {
				driftErr("%s: request field %q is accepted but not in the spec", pattern, f)
			}
		}
		for f := range props {
			if !fields[f] {
				driftErr("%s: spec lists request field %q but the handler rejects it", pattern, f)
			}
		}
	}
}

func keys(m map[string]bool) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// noCoverage lists routes the Go suite cannot produce a 2xx for, with why.
var noCoverage = map[string]string{}

func TestMain(m *testing.M) {
	code := m.Run()
	checkBodies()
	full := true
	for _, a := range os.Args[1:] {
		if strings.HasPrefix(a, "-test.run") || strings.HasPrefix(a, "-test.skip") {
			full = false
		}
	}
	if full && code == 0 {
		loadDriftSpec()
		for path, ops := range driftSpec.Paths {
			for method := range ops {
				p := strings.ToUpper(method) + " " + path
				if !driftSeen[p] && noCoverage[p] == "" {
					driftErr("%s: no test got a 2xx from this operation, so its schema is unchecked", p)
				}
			}
		}
	}
	if len(driftErrs) > 0 {
		list := make([]string, 0, len(driftErrs))
		for e := range driftErrs {
			list = append(list, e)
		}
		sort.Strings(list)
		fmt.Fprintf(os.Stderr, "\nOpenAPI drift (%d):\n  %s\n", len(list), strings.Join(list, "\n  "))
		code = 1
	}
	os.Exit(code)
}

// The checker itself must catch drift.
func TestDriftValidatorCatches(t *testing.T) {
	loadDriftSpec()
	s := map[string]any{"type": "object", "required": []any{"a"}, "properties": map[string]any{"a": map[string]any{"type": "integer"}}}
	if p := validate(map[string]any{"a": 1.0}, s, "x"); len(p) != 0 {
		t.Errorf("valid object rejected: %v", p)
	}
	for _, bad := range []any{map[string]any{}, map[string]any{"a": "s"}, map[string]any{"a": 1.5}, map[string]any{"a": 1.0, "extra": 1.0}, []any{}} {
		if p := validate(bad, s, "x"); len(p) == 0 {
			t.Errorf("%v accepted", bad)
		}
	}
	if p := validate("c", map[string]any{"enum": []any{"a", "b"}}, "x"); len(p) == 0 {
		t.Error("bad enum accepted")
	}
	if p := validate(nil, map[string]any{"type": []any{"integer", "null"}}, "x"); len(p) != 0 {
		t.Errorf("nullable rejected: %v", p)
	}
}
