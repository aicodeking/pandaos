package main

import (
	"encoding/json"
	"fmt"
	"io"
)

// kindDoc explains the memory kinds wherever a tool takes one.
const kindDoc = `episodic = what happened: a finished task, an incident, a debugging session (dated; prefer mem_log_episode). semantic = facts that stay true: how this box/project is set up, decisions and why, user preferences. procedural = how to do something: step-by-step recipes, commands, checklists.`

const toolsJSON = `[
{"name":"mem_write","description":"Create a shared memory (markdown, committed to git, visible to every agent on this box). The file name comes from the title, so a title names one memory. Replacing an existing memory needs overwrite:true; to add to one, use mem_append. Kinds: ` + kindDoc + ` Returns the path, e.g. semantic/nix-flakes-1a2b3c4d.md.","annotations":{"title":"Write memory","readOnlyHint":false,"destructiveHint":true,"idempotentHint":true,"openWorldHint":false},"inputSchema":{"type":"object","required":["kind","title","body"],"properties":{"kind":{"type":"string","enum":["episodic","semantic","procedural"],"description":"` + kindDoc + `"},"title":{"type":"string","description":"One line naming the memory; also its identity (same title = same memory)."},"body":{"type":"string","description":"Markdown content, up to 256 KiB."},"tags":{"type":"array","items":{"type":"string"},"description":"Optional keywords that help mem_search, e.g. [\"nix\",\"build\"]."},"overwrite":{"type":"boolean","default":false,"description":"Replace the memory with this title if it exists (git history keeps the old version). Without it, an existing title is an error."}}}},
{"name":"mem_append","description":"Add a timestamped section (### <UTC time>) to the end of an existing memory, found by path or by title. Use it to extend notes, procedures or decisions without replacing them.","annotations":{"title":"Append to memory","readOnlyHint":false,"destructiveHint":false,"idempotentHint":false,"openWorldHint":false},"inputSchema":{"type":"object","required":["text"],"properties":{"path":{"type":"string","description":"Memory path from mem_write/mem_search/mem_list, e.g. procedural/deploy-1a2b3c4d.md. Takes precedence over title."},"title":{"type":"string","description":"Exact title of the memory (any kind), used when path is not given."},"text":{"type":"string","description":"Markdown to append, up to 256 KiB."}}}},
{"name":"mem_delete","description":"Delete a memory for every agent and commit the removal. Git history still has it. Requires confirm:true. Prefer mem_append or mem_write overwrite:true to correct a memory.","annotations":{"title":"Delete memory","readOnlyHint":false,"destructiveHint":true,"idempotentHint":true,"openWorldHint":false},"inputSchema":{"type":"object","required":["path","confirm"],"properties":{"path":{"type":"string","description":"Memory path, e.g. semantic/old-note-1a2b3c4d.md."},"confirm":{"type":"boolean","description":"Must be true: you mean to remove this memory for all agents."}}}},
{"name":"mem_log_episode","description":"Record a finished task as an episodic memory episodic/<date>-<slug>.md: what was asked, the outcome, a summary of what was done and learned, and the files touched. Call this at the end of every task so later agents can find what was tried.","annotations":{"title":"Log episode","readOnlyHint":false,"destructiveHint":false,"idempotentHint":false,"openWorldHint":false},"inputSchema":{"type":"object","required":["task","summary","outcome"],"properties":{"task":{"type":"string","description":"One line: the task as given, e.g. \"Fix flaky login test\"."},"summary":{"type":"string","description":"Markdown: what you did, what worked and what did not, and anything the next agent should know."},"outcome":{"type":"string","enum":["success","partial","failure"],"description":"success = done as asked; partial = some of it done or with caveats; failure = not done."},"files":{"type":"array","items":{"type":"string"},"description":"Optional paths you created or changed (at most 200)."}}}},
{"name":"mem_search","description":"Case-insensitive search of all memories. The query is split into terms; results are ranked by distinct terms matched, then total hits, each with its best matching line. Search before starting work to reuse what other agents learned.","annotations":{"title":"Search memory","readOnlyHint":true,"destructiveHint":false,"idempotentHint":true,"openWorldHint":false},"inputSchema":{"type":"object","required":["query"],"properties":{"query":{"type":"string","description":"Words to look for, e.g. \"nix build cache\"."},"limit":{"type":"integer","minimum":1,"default":10,"description":"Maximum results (default 10)."},"kind":{"type":"string","enum":["episodic","semantic","procedural"],"description":"Only search this kind. ` + kindDoc + `"}}}},
{"name":"mem_read","description":"Read one memory file (YAML-ish front matter, then the markdown body) by path.","annotations":{"title":"Read memory","readOnlyHint":true,"destructiveHint":false,"idempotentHint":true,"openWorldHint":false},"inputSchema":{"type":"object","required":["path"],"properties":{"path":{"type":"string","description":"Memory path, e.g. semantic/foo-1a2b3c4d.md, from mem_search or mem_list."}}}},
{"name":"mem_list","description":"List memories (path, title, kind, created, tags), newest first.","annotations":{"title":"List memories","readOnlyHint":true,"destructiveHint":false,"idempotentHint":true,"openWorldHint":false},"inputSchema":{"type":"object","properties":{"kind":{"type":"string","enum":["episodic","semantic","procedural"],"description":"Only list this kind. ` + kindDoc + `"},"limit":{"type":"integer","minimum":1,"default":50,"description":"Maximum entries (default 50)."}}}},
{"name":"mem_history","description":"Git log of the memory store (short hash, time, message), optionally for one memory, to see who changed what and when.","annotations":{"title":"Memory history","readOnlyHint":true,"destructiveHint":false,"idempotentHint":true,"openWorldHint":false},"inputSchema":{"type":"object","properties":{"path":{"type":"string","description":"Only the history of this memory path."},"limit":{"type":"integer","minimum":1,"default":20,"description":"Maximum commits (default 20)."}}}}
]`

var mcpTools = json.RawMessage(toolsJSON)

type toolArgs struct {
	Kind      string   `json:"kind,omitempty"`
	Title     string   `json:"title,omitempty"`
	Body      string   `json:"body,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	Overwrite bool     `json:"overwrite,omitempty"`
	Text      string   `json:"text,omitempty"`
	Confirm   bool     `json:"confirm,omitempty"`
	Task      string   `json:"task,omitempty"`
	Summary   string   `json:"summary,omitempty"`
	Outcome   string   `json:"outcome,omitempty"`
	Files     []string `json:"files,omitempty"`
	Query     string   `json:"query,omitempty"`
	Path      string   `json:"path,omitempty"`
	Limit     int      `json:"limit,omitempty"`
}

func orDefault(n, d int) int {
	if n <= 0 {
		return d
	}
	return n
}

func (s *Store) runTool(name string, a toolArgs) (string, error) {
	switch name {
	case "mem_write":
		return s.Write(a.Kind, a.Title, a.Body, a.Tags, a.Overwrite)
	case "mem_append":
		return s.Append(a.Path, a.Title, a.Text)
	case "mem_delete":
		return s.Delete(a.Path, a.Confirm)
	case "mem_log_episode":
		return s.LogEpisode(a.Task, a.Summary, a.Outcome, a.Files)
	case "mem_search":
		return jsonText(s.Search(a.Query, a.Kind, orDefault(a.Limit, 10)))
	case "mem_read":
		return s.Read(a.Path)
	case "mem_list":
		return jsonText(s.List(a.Kind, orDefault(a.Limit, 50)))
	case "mem_history":
		return s.History(a.Path, orDefault(a.Limit, 20))
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

// ServeMCP speaks MCP over r/w until EOF.
func (s *Store) ServeMCP(r io.Reader, w io.Writer) error {
	srv := &mcpServer{
		Name:         "panda-mem",
		Version:      version,
		Instructions: "Shared memory for all agents on this box. mem_search before starting; mem_log_episode at the end of every task.",
		Tools:        mcpTools,
		Call: func(name string, raw json.RawMessage) (string, error) {
			var a toolArgs
			if err := decodeArgs(raw, &a); err != nil {
				return "", fmt.Errorf("bad arguments: %w", err)
			}
			return s.runTool(name, a)
		},
	}
	return srv.Serve(r, w)
}
