// MCP stdio plumbing (newline-delimited JSON-RPC 2.0).
//
// Same protocol as the shared mcpserve.go of mem/checkpoint/sysmcp, but tool
// results here are content lists (text plus PNG images), which that file's
// text-only Call cannot return, so this component keeps its own copy.

package main

import (
	"bufio"
	"encoding/base64"
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

var protocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

func negotiate(requested string) string {
	if slices.Contains(protocolVersions, requested) {
		return requested
	}
	return protocolVersions[0]
}

// content is one MCP content item: text or image.
type content map[string]string

func textContent(s string) content { return content{"type": "text", "text": s} }

func imageContent(png []byte) content {
	return content{"type": "image", "mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(png)}
}

type mcpServer struct {
	Name, Version, Instructions string
	Tools                       json.RawMessage
	Call                        func(name string, args json.RawMessage) ([]content, error)
}

func decodeArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, v)
}

func (s *mcpServer) callTool(params json.RawMessage) map[string]any {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	var out []content
	err := json.Unmarshal(params, &p)
	if err == nil {
		out, err = s.Call(p.Name, p.Arguments)
	}
	if err != nil {
		return map[string]any{"content": []content{textContent(err.Error())}, "isError": true}
	}
	return map[string]any{"content": out, "isError": false}
}

func (s *mcpServer) handle(req rpcReq) *rpcResp {
	resp := &rpcResp{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		resp.Result = map[string]any{
			"protocolVersion": negotiate(p.ProtocolVersion),
			"serverInfo":      map[string]string{"name": s.Name, "version": s.Version},
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"instructions":    s.Instructions,
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": s.Tools}
	case "tools/call":
		resp.Result = s.callTool(req.Params)
	default:
		resp.Error = &rpcErr{-32601, "method not found: " + req.Method}
	}
	if req.ID == nil {
		return nil
	}
	return resp
}

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
