package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Shared parameter text: a target is a ~/work workspace or a directory path.
const wsParam = `"workspace":{"type":"string","description":"A workspace name in ~/work (e.g. \"myproj\"), or the absolute or ~/ path of a directory elsewhere in your home, e.g. a git checkout (\"~/src/app\"). Paths are checkpointed by reflink copy and are never moved."}`

const toolsJSON = `[
{"name":"ckpt_create","description":"Checkpoint a workspace or directory: an instant, read-only copy-on-write copy of its current state. ALWAYS call this BEFORE risky changes: large refactors, deleting or moving files, mass edits, migrations, code generators, dependency upgrades, or running unfamiliar scripts. Returns the checkpoint (its id is what ckpt_diff, ckpt_restore and ckpt_fork take). Checkpoints protect against mistakes, not against a process running as you deliberately deleting them.","annotations":{"title":"Create checkpoint","readOnlyHint":false,"destructiveHint":false,"idempotentHint":false,"openWorldHint":false},"inputSchema":{"type":"object","required":["workspace"],"properties":{` + wsParam + `,"label":{"type":"string","description":"Short note on what you are about to try, e.g. \"before switching ORM\". Default: \"manual\"."}}}},
{"name":"ckpt_list","description":"List checkpoints of a workspace or directory, oldest first, each with id, created, label, reason (manual, auto, pre-restore), parent and sizeEstimate. Without workspace: the ~/work workspaces and the git repos found elsewhere in your home (with their checkpoint counts).","annotations":{"title":"List checkpoints","readOnlyHint":true,"destructiveHint":false,"idempotentHint":true,"openWorldHint":false},"inputSchema":{"type":"object","properties":{` + wsParam + `,"limit":{"type":"integer","minimum":1,"default":20,"description":"Return only the newest N checkpoints (still oldest first). Default 20."}}}},
{"name":"ckpt_diff","description":"Files added, deleted or modified between checkpoint id and id2 (default: the current state), with unified diffs for text files up to 64 KiB. Use it to see what changed since a checkpoint before deciding to restore.","annotations":{"title":"Diff checkpoint","readOnlyHint":true,"destructiveHint":false,"idempotentHint":true,"openWorldHint":false},"inputSchema":{"type":"object","required":["workspace","id"],"properties":{` + wsParam + `,"id":{"type":"string","description":"The older checkpoint id, from ckpt_list or ckpt_create."},"id2":{"type":"string","description":"The newer checkpoint id, or \"current\" (default) for the live files."}}}},
{"name":"ckpt_restore","description":"Replace the whole workspace or directory with checkpoint id. Everything changed since then is replaced, so a safety checkpoint of the current state is taken first and returned as safety (restore that id to undo). Requires confirm:true. Afterwards re-open files and cd into the directory again: processes still inside see the old tree.","annotations":{"title":"Restore checkpoint","readOnlyHint":false,"destructiveHint":true,"idempotentHint":false,"openWorldHint":false},"inputSchema":{"type":"object","required":["workspace","id","confirm"],"properties":{` + wsParam + `,"id":{"type":"string","description":"The checkpoint id to bring back, from ckpt_list."},"confirm":{"type":"boolean","description":"Must be true: you understand the current files are replaced (a safety checkpoint is kept)."}}}},
{"name":"ckpt_fork","description":"Create a new writable workspace ~/work/<name> from checkpoint id, so parallel attempts can start from the same state without touching each other. Works for checkpoints of ~/work workspaces and of directories elsewhere. Checkpoint the fork before risky changes too.","annotations":{"title":"Fork checkpoint","readOnlyHint":false,"destructiveHint":false,"idempotentHint":false,"openWorldHint":false},"inputSchema":{"type":"object","required":["workspace","id","name"],"properties":{` + wsParam + `,"id":{"type":"string","description":"The checkpoint id to start from."},"name":{"type":"string","description":"Name of the new workspace in ~/work: [A-Za-z0-9._-]{1,64}, not starting with '.', must not exist yet."}}}},
{"name":"ckpt_init","description":"Create a new, empty workspace ~/work/<workspace> as a btrfs subvolume, so its checkpoints are instant snapshots. Use it for new projects; for an existing directory use ckpt_adopt (inside ~/work) or just pass its path to ckpt_create (elsewhere).","annotations":{"title":"Create workspace","readOnlyHint":false,"destructiveHint":false,"idempotentHint":false,"openWorldHint":false},"inputSchema":{"type":"object","required":["workspace"],"properties":{"workspace":{"type":"string","description":"New workspace name: [A-Za-z0-9._-]{1,64}, not starting with '.', must not exist yet in ~/work."}}}},
{"name":"ckpt_adopt","description":"Convert an existing plain directory ~/work/<name> into a subvolume workspace in place (new subvolume, reflink copy, atomic swap, old tree removed). Refused while any of your processes has its cwd or open files inside it, because they would keep using the old, deleted tree; then stop them or pass confirm:true and cd back in afterwards. Directories outside ~/work are not adopted: pass their path to ckpt_create instead.","annotations":{"title":"Adopt directory","readOnlyHint":false,"destructiveHint":false,"idempotentHint":false,"openWorldHint":false},"inputSchema":{"type":"object","required":["path"],"properties":{"path":{"type":"string","description":"The directory, directly inside ~/work: ~/work/<name> or its absolute path."},"confirm":{"type":"boolean","default":false,"description":"Adopt even though processes are using the directory (they must cd back in afterwards)."}}}},
{"name":"ckpt_discover","description":"Find git repos in your home outside ~/work (up to 4 levels deep, skipping caches) that panda-ckpt checkpoints automatically by reflink copy, with their checkpoint counts. Their paths are valid workspace values for every ckpt_ tool.","annotations":{"title":"Discover repos","readOnlyHint":true,"destructiveHint":false,"idempotentHint":true,"openWorldHint":false},"inputSchema":{"type":"object","properties":{}}}
]`

var mcpTools = json.RawMessage(toolsJSON)

type toolArgs struct {
	Workspace string `json:"workspace"`
	Label     string `json:"label"`
	ID        string `json:"id"`
	ID2       string `json:"id2"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	Limit     int    `json:"limit"`
	Confirm   bool   `json:"confirm"`
}

func (m *Manager) runTool(name string, a toolArgs) (any, error) {
	switch name {
	case "ckpt_create":
		return m.Create(a.Workspace, a.Label)
	case "ckpt_list":
		if a.Workspace == "" {
			return m.Targets()
		}
		if a.Limit <= 0 {
			a.Limit = 20
		}
		list, err := m.List(a.Workspace)
		return lastN(list, a.Limit), err
	case "ckpt_diff":
		return m.Diff(a.Workspace, a.ID, a.ID2)
	case "ckpt_restore":
		if !a.Confirm {
			return nil, errors.New("ckpt_restore replaces the workspace; call again with confirm:true")
		}
		return m.Restore(a.Workspace, a.ID)
	case "ckpt_fork":
		return m.Fork(a.Workspace, a.ID, a.Name)
	case "ckpt_init":
		return m.Init(a.Workspace)
	case "ckpt_adopt":
		return m.Adopt(a.Path, a.Confirm)
	case "ckpt_discover":
		return m.Discover()
	}
	return nil, fmt.Errorf("unknown tool %q", name)
}

// ServeMCP speaks MCP over r/w until EOF.
func (m *Manager) ServeMCP(r io.Reader, w io.Writer) error {
	s := &mcpServer{
		Name:         "panda-ckpt",
		Version:      version,
		Instructions: "Workspaces live in ~/work; git repos elsewhere in your home can be checkpointed by path. Call ckpt_create before any risky change; ckpt_diff shows what changed; ckpt_restore (confirm:true) rolls back; ckpt_fork branches.",
		Tools:        mcpTools,
		Call: func(name string, raw json.RawMessage) (string, error) {
			var a toolArgs
			if err := decodeArgs(raw, &a); err != nil {
				return "", fmt.Errorf("bad arguments: %w", err)
			}
			return jsonText(m.runTool(name, a))
		},
	}
	return s.Serve(r, w)
}
