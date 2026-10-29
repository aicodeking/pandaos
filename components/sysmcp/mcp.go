package main

import (
	"bufio"
	"encoding/json"
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
{"name":"sys_status","description":"Hostname, uptime, load, CPU count, memory, disk free per mount, PandaOS version, battery.","inputSchema":{"type":"object","properties":{}}},
{"name":"sys_services","description":"State of the PandaOS units plus systemctl --failed.","inputSchema":{"type":"object","properties":{}}},
{"name":"sys_logs","description":"journalctl output. Non-wheel callers are limited to the PandaOS units.","inputSchema":{"type":"object","properties":{"unit":{"type":"string","description":"e.g. panda-mem or nixos-upgrade.service"},"since":{"type":"string","description":"journalctl --since, e.g. '1h ago' or '2026-10-29 10:00'"},"lines":{"type":"integer","minimum":1,"maximum":500,"default":100},"grep":{"type":"string","maxLength":200}}}},
{"name":"sys_top","description":"Top processes by CPU and by memory.","inputSchema":{"type":"object","properties":{"n":{"type":"integer","minimum":1,"maximum":50,"default":10}}}},
{"name":"sys_net","description":"Interfaces, default route, DNS, and a HEAD connectivity check to the configured URL.","inputSchema":{"type":"object","properties":{}}},
{"name":"sys_updates","description":"panda-update status; action=run (mutating) needs confirm:true, allowMutations, and a wheel caller.","inputSchema":{"type":"object","properties":{"action":{"type":"string","enum":["status","run"],"default":"status"},"confirm":{"type":"boolean","default":false}}}},
{"name":"sys_ports","description":"Listening TCP/UDP sockets.","inputSchema":{"type":"object","properties":{}}},
{"name":"sys_ckpt_space","description":"btrfs usage of / and /.snapshots (falls back to df).","inputSchema":{"type":"object","properties":{}}}
]`

var tools = json.RawMessage(toolsJSON)

func textResult(text string, isErr bool) map[string]any {
	return map[string]any{"content": []map[string]string{{"type": "text", "text": text}}, "isError": isErr}
}

func (s *Server) callTool(params json.RawMessage) map[string]any {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	var a toolArgs
	if err := json.Unmarshal(params, &p); err != nil {
		return textResult("bad params: "+err.Error(), true)
	}
	if len(p.Arguments) > 0 && string(p.Arguments) != "null" {
		if err := json.Unmarshal(p.Arguments, &a); err != nil {
			return textResult("bad arguments: "+err.Error(), true)
		}
	}
	out, err := s.runTool(p.Name, a)
	if err != nil {
		return textResult(err.Error(), true)
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return textResult(string(b), false)
}

func (s *Server) handle(req rpcReq) *rpcResp {
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
			"serverInfo":      map[string]string{"name": "panda-sys", "version": version},
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
func (s *Server) ServeMCP(r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
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

// rawJSON embeds s as JSON if it is valid, else as a string.
func rawJSON(s string) any {
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	return s
}
