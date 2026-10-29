// Shared MCP stdio plumbing (newline-delimited JSON-RPC 2.0).
//
// This file is byte-identical in components/mem, components/checkpoint and
// components/sysmcp. Each is its own Go module built by buildGoModule with
// src = ./. and no vendoring, so a shared package would need a fileset src
// plus vendoring; copying one small file is cheaper. mcpserve_test.go fails
// when a sibling copy differs (it skips where siblings are absent, e.g. in
// the Nix build sandbox).

package main

import (
	"bufio"
	"encoding/json"
	"io"
	"slices"
)

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

// protocolVersions are the MCP revisions this server speaks, newest first.
var protocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// negotiate echoes the client's version if supported, else offers the newest.
func negotiate(requested string) string {
	if slices.Contains(protocolVersions, requested) {
		return requested
	}
	return protocolVersions[0]
}

// mcpServer serves Tools (the tools/list JSON array) and dispatches
// tools/call to Call, whose text (or error) becomes the tool result.
type mcpServer struct {
	Name         string
	Version      string
	Instructions string
	Tools        json.RawMessage
	Call         func(name string, args json.RawMessage) (string, error)
}

func textResult(text string, isErr bool) map[string]any {
	return map[string]any{"content": []map[string]string{{"type": "text", "text": text}}, "isError": isErr}
}

// decodeArgs unmarshals tool arguments; absent or null means all defaults.
func decodeArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, v)
}

// jsonText renders a tool's structured result as indented JSON text.
func jsonText(v any, err error) (string, error) {
	if err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	return string(b), err
}

func (s *mcpServer) callTool(params json.RawMessage) map[string]any {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return textResult("bad params: "+err.Error(), true)
	}
	out, err := s.Call(p.Name, p.Arguments)
	if err != nil {
		return textResult(err.Error(), true)
	}
	return textResult(out, false)
}

func (s *mcpServer) handle(req rpcReq) *rpcResp {
	resp := &rpcResp{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		res := map[string]any{
			"protocolVersion": negotiate(p.ProtocolVersion),
			"serverInfo":      map[string]string{"name": s.Name, "version": s.Version},
			"capabilities":    map[string]any{"tools": map[string]any{}},
		}
		if s.Instructions != "" {
			res["instructions"] = s.Instructions
		}
		resp.Result = res
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": s.Tools}
	case "tools/call":
		resp.Result = s.callTool(req.Params)
	default:
		resp.Error = &rpcErr{-32601, "method not found: " + req.Method}
	}
	if req.ID == nil { // notification (incl. notifications/initialized): never reply
		return nil
	}
	return resp
}

// Serve speaks newline-delimited JSON-RPC 2.0 until r hits EOF.
func (s *mcpServer) Serve(r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	enc := json.NewEncoder(w)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcReq
		var resp *rpcResp
		if err := json.Unmarshal(line, &req); err != nil {
			resp = &rpcResp{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcErr{-32700, "parse error"}}
		} else {
			resp = s.handle(req)
		}
		if resp != nil {
			if err := enc.Encode(resp); err != nil {
				return err
			}
		}
	}
	return sc.Err()
}
