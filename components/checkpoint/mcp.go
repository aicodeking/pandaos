package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

const toolsJSON = `[
{"name":"ckpt_create","description":"Checkpoint a workspace (~/work/<workspace>): an instant, read-only copy-on-write snapshot. ALWAYS call this BEFORE risky changes: large refactors, deleting or moving files, mass edits, migrations, code generators, dependency upgrades, or running unfamiliar scripts. Returns the checkpoint id; ckpt_restore can bring this state back.","inputSchema":{"type":"object","required":["workspace"],"properties":{"workspace":{"type":"string"},"label":{"type":"string","description":"what you are about to try"}}}},
{"name":"ckpt_list","description":"List checkpoints of a workspace, oldest first (reason: manual, auto, pre-restore). Without workspace, lists the workspaces.","inputSchema":{"type":"object","properties":{"workspace":{"type":"string"}}}},
{"name":"ckpt_diff","description":"Files added/deleted/modified between checkpoint id and id2 (default: the current workspace), with unified diffs for small text files.","inputSchema":{"type":"object","required":["workspace","id"],"properties":{"workspace":{"type":"string"},"id":{"type":"string"},"id2":{"type":"string","description":"checkpoint id or \"current\""}}}},
{"name":"ckpt_restore","description":"Replace the whole workspace with checkpoint id. Uncommitted work since then is replaced, so a safety checkpoint of the current state is taken first (returned as safety). Requires confirm:true. Afterwards re-open files and cd into the workspace again.","inputSchema":{"type":"object","required":["workspace","id","confirm"],"properties":{"workspace":{"type":"string"},"id":{"type":"string"},"confirm":{"type":"boolean"}}}},
{"name":"ckpt_fork","description":"Create a new writable workspace <name> from checkpoint id of workspace, so parallel attempts can start from the same state without touching each other. Checkpoint the fork before risky changes too.","inputSchema":{"type":"object","required":["workspace","id","name"],"properties":{"workspace":{"type":"string"},"id":{"type":"string"},"name":{"type":"string"}}}}
]`

var tools = json.RawMessage(toolsJSON)

type toolArgs struct {
	Workspace string `json:"workspace"`
	Label     string `json:"label"`
	ID        string `json:"id"`
	ID2       string `json:"id2"`
	Name      string `json:"name"`
	Confirm   bool   `json:"confirm"`
}

func textResult(text string, isErr bool) map[string]any {
	return map[string]any{"content": []map[string]string{{"type": "text", "text": text}}, "isError": isErr}
}

func (m *Manager) runTool(name string, a toolArgs) (any, error) {
	switch name {
	case "ckpt_create":
		return m.Create(a.Workspace, a.Label)
	case "ckpt_list":
		if a.Workspace == "" {
			return m.Workspaces()
		}
		return m.List(a.Workspace)
	case "ckpt_diff":
		return m.Diff(a.Workspace, a.ID, a.ID2)
	case "ckpt_restore":
		if !a.Confirm {
			return nil, errors.New("ckpt_restore replaces the workspace; call again with confirm:true")
		}
		return m.Restore(a.Workspace, a.ID)
	case "ckpt_fork":
		return m.Fork(a.Workspace, a.ID, a.Name)
	}
	return nil, fmt.Errorf("unknown tool %q", name)
}

func (m *Manager) callTool(params json.RawMessage) map[string]any {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	var a toolArgs
	if err := json.Unmarshal(params, &p); err != nil {
		return textResult("bad params: "+err.Error(), true)
	}
	if len(p.Arguments) > 0 {
		if err := json.Unmarshal(p.Arguments, &a); err != nil {
			return textResult("bad arguments: "+err.Error(), true)
		}
	}
	v, err := m.runTool(p.Name, a)
	if err != nil {
		return textResult(err.Error(), true)
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	return textResult(string(b), false)
}

func (m *Manager) handle(req rpcReq) *rpcResp {
	resp := &rpcResp{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-06-18"
		}
		resp.Result = map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"serverInfo":      map[string]string{"name": "panda-ckpt", "version": version},
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"instructions":    "Workspaces live in ~/work. Call ckpt_create before any risky change; ckpt_restore (confirm:true) rolls back, ckpt_fork branches.",
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": tools}
	case "tools/call":
		resp.Result = m.callTool(req.Params)
	default:
		resp.Error = &rpcErr{-32601, "method not found: " + req.Method}
	}
	if req.ID == nil { // notification: never reply
		return nil
	}
	return resp
}

// ServeMCP speaks newline-delimited JSON-RPC 2.0 until r hits EOF.
func (m *Manager) ServeMCP(r io.Reader, w io.Writer) error {
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
			resp = m.handle(req)
		}
		if resp != nil {
			if err := enc.Encode(resp); err != nil {
				return err
			}
		}
	}
	return sc.Err()
}
