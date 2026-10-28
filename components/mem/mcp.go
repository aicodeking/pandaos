package main

import (
	"bufio"
	"encoding/json"
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
{"name":"mem_write","description":"Write (or update, by title) a memory. Returns its path.","inputSchema":{"type":"object","required":["kind","title","body"],"properties":{"kind":{"type":"string","enum":["episodic","semantic","procedural"]},"title":{"type":"string"},"body":{"type":"string"},"tags":{"type":"array","items":{"type":"string"}}}}},
{"name":"mem_search","description":"Case-insensitive substring search over memories, ranked by match count.","inputSchema":{"type":"object","required":["query"],"properties":{"query":{"type":"string"},"limit":{"type":"integer","default":10},"kind":{"type":"string","enum":["episodic","semantic","procedural"]}}}},
{"name":"mem_read","description":"Read a memory file by path (e.g. semantic/foo-1a2b3c4d.md).","inputSchema":{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}},
{"name":"mem_list","description":"List memories, newest first.","inputSchema":{"type":"object","properties":{"kind":{"type":"string","enum":["episodic","semantic","procedural"]},"limit":{"type":"integer","default":50}}}},
{"name":"mem_history","description":"Git log of the memory store, optionally for one path.","inputSchema":{"type":"object","properties":{"path":{"type":"string"},"limit":{"type":"integer","default":20}}}}
]`

var tools = json.RawMessage(toolsJSON)

type toolArgs struct {
	Kind  string   `json:"kind"`
	Title string   `json:"title"`
	Body  string   `json:"body"`
	Tags  []string `json:"tags"`
	Query string   `json:"query"`
	Path  string   `json:"path"`
	Limit int      `json:"limit"`
}

func textResult(text string, isErr bool) map[string]any {
	return map[string]any{"content": []map[string]string{{"type": "text", "text": text}}, "isError": isErr}
}

func jsonText(v any, err error) (string, error) {
	if err != nil {
		return "", err
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b), nil
}

func orDefault(n, d int) int {
	if n <= 0 {
		return d
	}
	return n
}

func (s *Store) callTool(params json.RawMessage) map[string]any {
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
	var out string
	var err error
	switch p.Name {
	case "mem_write":
		out, err = s.Write(a.Kind, a.Title, a.Body, a.Tags)
	case "mem_search":
		out, err = jsonText(s.Search(a.Query, a.Kind, orDefault(a.Limit, 10)))
	case "mem_read":
		out, err = s.Read(a.Path)
	case "mem_list":
		out, err = jsonText(s.List(a.Kind, orDefault(a.Limit, 50)))
	case "mem_history":
		out, err = s.History(a.Path, orDefault(a.Limit, 20))
	default:
		err = fmt.Errorf("unknown tool %q", p.Name)
	}
	if err != nil {
		return textResult(err.Error(), true)
	}
	return textResult(out, false)
}

func (s *Store) handle(req rpcReq) *rpcResp {
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
			"serverInfo":      map[string]string{"name": "panda-mem", "version": version},
			"capabilities":    map[string]any{"tools": map[string]any{}},
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": tools}
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

// ServeMCP speaks newline-delimited JSON-RPC 2.0 until r hits EOF.
func (s *Store) ServeMCP(r io.Reader, w io.Writer) error {
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
