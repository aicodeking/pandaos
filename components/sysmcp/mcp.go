package main

import (
	"encoding/json"
	"fmt"
	"io"
)

const ro = `"annotations":{"readOnlyHint":true,"destructiveHint":false,"idempotentHint":true,"openWorldHint":false}`

const toolsJSON = `[
{"name":"sys_status","description":"Hostname, uptime, load, CPU count, memory, disk free per mount, PandaOS version and battery. Check this before heavy builds.",` + ro + `,"inputSchema":{"type":"object","properties":{}}},
{"name":"sys_services","description":"State of the PandaOS units (agents, memory, checkpoints, upgrades, network) plus systemctl --failed.",` + ro + `,"inputSchema":{"type":"object","properties":{}}},
{"name":"sys_logs","description":"journalctl output, newest last. Non-wheel callers are limited to the PandaOS units listed by sys_services.",` + ro + `,"inputSchema":{"type":"object","properties":{"unit":{"type":"string","description":"Unit to read, e.g. panda-mem or nixos-upgrade.service (.service is implied). Default: all units you may read."},"since":{"type":"string","description":"journalctl --since, e.g. '1h ago', 'today' or '2026-10-29 10:00'."},"lines":{"type":"integer","minimum":1,"maximum":500,"default":100,"description":"Number of most recent lines, 1..500 (default 100)."},"grep":{"type":"string","maxLength":200,"description":"Only lines matching this journalctl --grep pattern (PCRE, case-insensitive when all lowercase)."}}}},
{"name":"sys_top","description":"Top processes by CPU and by memory (pid, user, cpu%, mem%, RSS, command).",` + ro + `,"inputSchema":{"type":"object","properties":{"n":{"type":"integer","minimum":1,"maximum":50,"default":10,"description":"Processes per list, 1..50 (default 10)."}}}},
{"name":"sys_net","description":"Interfaces, default route, DNS, and a HEAD request to the configured connectivity URL (is the internet reachable?).","annotations":{"readOnlyHint":true,"destructiveHint":false,"idempotentHint":true,"openWorldHint":true},"inputSchema":{"type":"object","properties":{}}},
{"name":"sys_updates","description":"System upgrade (nixos-upgrade.service). action=status (default): unit state from systemctl show and whether updates are on hold. action=run starts an upgrade in the background and returns at once; poll status to follow it. run needs services.panda-sys.allowMutations, confirm:true, and a caller in group panda-sys-update (granted by a polkit rule for that one unit).","annotations":{"readOnlyHint":false,"destructiveHint":false,"idempotentHint":true,"openWorldHint":true},"inputSchema":{"type":"object","properties":{"action":{"type":"string","enum":["status","run"],"default":"status","description":"status = report only; run = start nixos-upgrade.service (switches the system to the latest configuration, health-checked with automatic rollback)."},"confirm":{"type":"boolean","default":false,"description":"Must be true for action=run."}}}},
{"name":"sys_ports","description":"Listening TCP and UDP sockets (protocol, state, local address:port).",` + ro + `,"inputSchema":{"type":"object","properties":{}}},
{"name":"sys_ckpt_space","description":"btrfs space usage of / and /.snapshots, where snapshots and checkpoints use space (falls back to df).",` + ro + `,"inputSchema":{"type":"object","properties":{}}}
]`

var mcpTools = json.RawMessage(toolsJSON)

// ServeMCP speaks MCP over r/w until EOF.
func (s *Server) ServeMCP(r io.Reader, w io.Writer) error {
	srv := &mcpServer{
		Name:    "panda-sys",
		Version: version,
		Tools:   mcpTools,
		Call: func(name string, raw json.RawMessage) (string, error) {
			var a toolArgs
			if err := decodeArgs(raw, &a); err != nil {
				return "", fmt.Errorf("bad arguments: %w", err)
			}
			return jsonText(s.runTool(name, a))
		},
	}
	return srv.Serve(r, w)
}

// rawJSON embeds s as JSON if it is valid, else as a string.
func rawJSON(s string) any {
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	return s
}
