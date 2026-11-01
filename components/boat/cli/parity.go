package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
)

func init() {
	for k, v := range map[string]func(*app, []string) error{
		"cmd": cmdCommands, "files": cmdFiles, "prompts": cmdPrompts, "conversations": cmdConversations,
		"steer": cmdSteer, "interrupt": cmdInterrupt, "env": cmdEnv, "resize": cmdResize,
		"ai": cmdAI, "telemetry": cmdTelemetry, "info": cmdInfo, "delete": cmdRm, "resume": cmdResume,
		"ps": cmdPS, "vm-usage": cmdVMUsage,
	} {
		commands[k] = v
	}
}

var (
	cmdCols    = []string{"id", "state", "exit", "started_at", "cmd"}
	promptCols = []string{"id", "state", "agent", "conversation_id", "prompt"}
)

func vmArg(a *app, ref string) (string, error) {
	vm, err := a.c.resolveVM(ref)
	if err != nil {
		return "", err
	}
	return s(vm, "id"), nil
}

// show prints a one-object response: JSON with --json, else a table row.
func (a *app) show(v obj, cols ...string) error { return a.emitList([]obj{v}, cols...) }

func (a *app) okLine(v obj, text string) error {
	return a.emit(v, func() { fmt.Fprintln(a.out, text) })
}

// followLogs streams a command's output to the end, reconnecting from the last
// offset when the stream asks for it, and returns the remote exit status.
func (a *app) followLogs(base string) error {
	var offset int64
	for {
		resp, err := a.c.request("GET", base+"?follow=1&offset="+strconv.FormatInt(offset, 10), nil, "")
		if err != nil {
			return err
		}
		done, code, err := a.readFollow(resp.Body, &offset)
		resp.Body.Close()
		if err != nil {
			return err
		}
		if done {
			if code != 0 {
				return exitError{code}
			}
			return nil
		}
	}
}

func (a *app) readFollow(body io.Reader, offset *int64) (done bool, code int, err error) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var ev struct {
			Stream     string `json:"stream"`
			DataB64    string `json:"data_b64"`
			NextOffset int64  `json:"next_offset"`
			Exit       *int   `json:"exit"`
			Signal     string `json:"signal"`
			Reconnect  bool   `json:"reconnect"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if a.json {
			fmt.Fprintln(a.out, sc.Text())
		} else if ev.DataB64 != "" {
			data, _ := base64.StdEncoding.DecodeString(ev.DataB64)
			a.out.Write(data)
		}
		if ev.NextOffset > *offset {
			*offset = ev.NextOffset
		}
		switch {
		case ev.Reconnect:
			return false, 0, errors.New("the VM stopped answering; output so far is shown")
		case ev.Exit != nil:
			return true, *ev.Exit, nil
		case ev.Signal != "":
			return true, 128, nil
		}
	}
	if err := sc.Err(); err != nil {
		return false, 0, err
	}
	return true, 0, nil // stream ended with neither: exit status unknown (killed)
}

// ---- detached commands ----

func cmdCommands(a *app, args []string) error {
	op, args := sub(args, "ls", "start", "ls", "status", "logs", "signal", "stop", "history")
	fs := flag.NewFlagSet("cmd", flag.ContinueOnError)
	follow := fs.Bool("f", false, "follow until the command ends")
	fs.BoolVar(follow, "follow", false, "")
	offset := fs.Int64("offset", 0, "")
	sig := fs.String("signal", "TERM", "")
	cwd := fs.String("cwd", "", "")
	pos, argv, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return fmt.Errorf("expected <vm>: %w", errUsage)
	}
	id, err := vmArg(a, pos[0])
	if err != nil {
		return err
	}
	base := vmPath(id, "commands")
	cmdPath := func() (string, error) {
		if len(pos) != 2 {
			return "", fmt.Errorf("expected <vm> <command-id>: %w", errUsage)
		}
		return base + "/" + url.PathEscape(pos[1]), nil
	}
	switch op {
	case "start":
		if len(argv) == 0 {
			return fmt.Errorf("usage: boat cmd start <vm> -- cmd [args]: %w", errUsage)
		}
		var out obj
		if err := a.c.call("POST", base, obj{"cmd": argv, "cwd": *cwd}, &out); err != nil {
			return err
		}
		if *follow {
			return a.followLogs(base + "/" + url.PathEscape(s(out, "id")) + "/logs")
		}
		return a.show(out, "id", "state", "cmd")
	case "ls":
		l, err := listCall(a, base, "commands")
		if err != nil {
			return err
		}
		return a.emitList(l, cmdCols...)
	case "history":
		l, err := listCall(a, vmPath(id, "command-history"), "commands")
		if err != nil {
			return err
		}
		return a.emitList(l, "id", "kind", "state", "exit", "started_at", "cmd")
	case "status":
		p, err := cmdPath()
		if err != nil {
			return err
		}
		var out obj
		if err := a.c.call("GET", p, nil, &out); err != nil {
			return err
		}
		return a.show(out, cmdCols...)
	case "logs":
		p, err := cmdPath()
		if err != nil {
			return err
		}
		return a.readLogs(p+"/logs", *follow, *offset)
	case "signal":
		p, err := cmdPath()
		if err != nil {
			return err
		}
		var out obj
		if err := a.c.call("POST", p+"/signal", obj{"signal": *sig}, &out); err != nil {
			return err
		}
		return a.okLine(out, "sent "+s(out, "signal")+" to "+s(out, "id"))
	default: // stop
		p, err := cmdPath()
		if err != nil {
			return err
		}
		var out obj
		if err := a.c.call("DELETE", p, nil, &out); err != nil {
			return err
		}
		return a.okLine(out, "stopped "+s(out, "id"))
	}
}

// readLogs prints output once, or follows it.
func (a *app) readLogs(base string, follow bool, offset int64) error {
	if follow {
		return a.followLogs(base)
	}
	var out obj
	if err := a.c.call("GET", base+"?offset="+strconv.FormatInt(offset, 10), nil, &out); err != nil {
		return err
	}
	return a.emit(out, func() {
		data, _ := base64.StdEncoding.DecodeString(s(out, "data_b64"))
		a.out.Write(data)
	})
}

func cmdPS(a *app, args []string) error {
	id, err := oneVM(a, args)
	if err != nil {
		return err
	}
	l, err := listCall(a, vmPath(s(id, "id"), "processes"), "processes")
	if err != nil {
		return err
	}
	return a.emitList(l, "pid", "user", "elapsed_s", "command")
}

// ---- files ----

func cmdFiles(a *app, args []string) error {
	op, args := sub(args, "ls", "ls", "rm", "mv", "search", "info", "mkdir", "chmod")
	fs := flag.NewFlagSet("files", flag.ContinueOnError)
	recursive := fs.Bool("r", false, "")
	overwrite := fs.Bool("overwrite", false, "")
	content := fs.Bool("content", false, "search file contents")
	regex := fs.Bool("regex", false, "")
	limit := fs.Int("limit", 0, "")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	want := map[string]int{"ls": 2, "rm": 2, "mv": 3, "search": 3, "info": 2, "mkdir": 2, "chmod": 3}[op]
	if len(pos) != want {
		return fmt.Errorf("usage: boat files %s <vm> ...: %w", op, errUsage)
	}
	id, err := vmArg(a, pos[0])
	if err != nil {
		return err
	}
	base := vmPath(id, "files")
	var out obj
	switch op {
	case "ls":
		l, err := listCall(a, base+"/list?path="+url.QueryEscape(pos[1]), "entries")
		if err != nil {
			return err
		}
		return a.emitList(l, "name", "type", "size", "mode")
	case "rm":
		q := "?path=" + url.QueryEscape(pos[1])
		if *recursive {
			q += "&recursive=1"
		}
		if err := a.c.call("DELETE", base+q, nil, &out); err != nil {
			return err
		}
		return a.okLine(out, "removed "+pos[1])
	case "mv":
		if err := a.c.call("POST", base+"/move", obj{"from": pos[1], "to": pos[2], "overwrite": *overwrite}, &out); err != nil {
			return err
		}
		return a.okLine(out, pos[1]+" -> "+pos[2])
	case "search":
		q := url.Values{"path": {pos[1]}, "q": {pos[2]}}
		if *content {
			q.Set("content", "1")
		}
		if *regex {
			q.Set("regex", "1")
		}
		if *limit > 0 {
			q.Set("limit", strconv.Itoa(*limit))
		}
		l, err := listCall(a, base+"/search?"+q.Encode(), "matches")
		if err != nil {
			return err
		}
		return a.emitList(l, "path", "line", "text")
	case "info":
		if err := a.c.call("GET", base+"/info?path="+url.QueryEscape(pos[1]), nil, &out); err != nil {
			return err
		}
		return a.show(out, "path", "type", "size", "mode")
	case "mkdir":
		if err := a.c.call("POST", base+"/folder", obj{"path": pos[1]}, &out); err != nil {
			return err
		}
		return a.okLine(out, "created "+pos[1])
	default:
		if err := a.c.call("POST", base+"/permissions", obj{"path": pos[2], "mode": pos[1]}, &out); err != nil {
			return err
		}
		return a.okLine(out, pos[2]+" mode "+pos[1])
	}
}

// ---- async prompts ----

type promptFlags struct {
	async, follow, fresh bool
	resume               string
}

func (f *promptFlags) register(fs *flag.FlagSet) {
	fs.BoolVar(&f.async, "async", false, "start a background turn and print its id")
	fs.BoolVar(&f.fresh, "new", false, "start a new conversation")
	fs.StringVar(&f.resume, "resume", "", "continue this conversation id")
}

func (f *promptFlags) used() bool { return f.async || f.fresh || f.resume != "" }

// startTurn is POST /prompts; without --async it follows the turn to its end.
func (a *app) startTurn(id string, body obj, f *promptFlags) error {
	body["new"], body["conversation_id"] = f.fresh, f.resume
	var run obj
	if err := a.c.call("POST", vmPath(id, "prompts"), body, &run); err != nil {
		return err
	}
	if f.async {
		return a.emit(run, func() {
			fmt.Fprintf(a.out, "prompt: %s\nconversation: %s\n", s(run, "id"), s(run, "conversation_id"))
		})
	}
	fmt.Fprintf(a.errw, "conversation: %s\n", s(run, "conversation_id"))
	return a.followLogs(vmPath(id, "prompts", s(run, "id"), "logs"))
}

func cmdPrompts(a *app, args []string) error {
	op, args := sub(args, "ls", "ls", "status", "logs")
	fs := flag.NewFlagSet("prompts", flag.ContinueOnError)
	follow := fs.Bool("f", false, "")
	fs.BoolVar(follow, "follow", false, "")
	offset := fs.Int64("offset", 0, "")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return fmt.Errorf("expected <vm>: %w", errUsage)
	}
	id, err := vmArg(a, pos[0])
	if err != nil {
		return err
	}
	if op == "ls" {
		l, err := listCall(a, vmPath(id, "prompts"), "prompts")
		if err != nil {
			return err
		}
		return a.emitList(l, promptCols...)
	}
	if err := need(pos, 2, "<vm> <prompt-id>"); err != nil {
		return err
	}
	p := vmPath(id, "prompts", url.PathEscape(pos[1]))
	if op == "logs" {
		return a.readLogs(p+"/logs", *follow, *offset)
	}
	var out obj
	if err := a.c.call("GET", p, nil, &out); err != nil {
		return err
	}
	return a.show(out, promptCols...)
}

func cmdConversations(a *app, args []string) error {
	vm, err := oneVM(a, args)
	if err != nil {
		return err
	}
	l, err := listCall(a, vmPath(s(vm, "id"), "conversations"), "conversations")
	if err != nil {
		return err
	}
	return a.emitList(l, "id", "agent", "prompt_count", "running", "last_prompt")
}

// runningPrompt finds the turn to act on: the given id, or the newest running.
func (a *app) runningPrompt(id, given string) (string, error) {
	if given != "" {
		return given, nil
	}
	l, err := listCall(a, vmPath(id, "prompts"), "prompts")
	if err != nil {
		return "", err
	}
	for i := len(l) - 1; i >= 0; i-- {
		if s(l[i], "state") == "running" {
			return s(l[i], "id"), nil
		}
	}
	return "", errors.New("no turn is running on this VM")
}

// cmdSteer interrupts the running turn and resumes its session with the message.
func cmdSteer(a *app, args []string) error {
	fs := flag.NewFlagSet("steer", flag.ContinueOnError)
	turn := fs.String("prompt-id", "", "the turn (default: the newest running one)")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 2, `<vm> "message"`); err != nil {
		return err
	}
	id, err := vmArg(a, pos[0])
	if err != nil {
		return err
	}
	pid, err := a.runningPrompt(id, *turn)
	if err != nil {
		return err
	}
	var out obj
	if err := a.c.call("POST", vmPath(id, "prompts", url.PathEscape(pid), "steer"), obj{"prompt": pos[1]}, &out); err != nil {
		return err
	}
	return a.show(out, promptCols...)
}

func cmdInterrupt(a *app, args []string) error {
	fs := flag.NewFlagSet("interrupt", flag.ContinueOnError)
	turn := fs.String("prompt-id", "", "the turn (default: the newest running one)")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 1, "<vm>"); err != nil {
		return err
	}
	id, err := vmArg(a, pos[0])
	if err != nil {
		return err
	}
	pid, err := a.runningPrompt(id, *turn)
	if err != nil {
		return err
	}
	var out obj
	if err := a.c.call("POST", vmPath(id, "prompts", url.PathEscape(pid), "interrupt"), nil, &out); err != nil {
		return err
	}
	return a.show(out, promptCols...)
}

// ---- environments ----

func cmdEnv(a *app, args []string) error {
	op, args := sub(args, "ls", "ls", "new", "show", "set", "rm", "build", "default")
	fs := flag.NewFlagSet("env", flag.ContinueOnError)
	image := fs.String("image", "", "")
	size := fs.String("size", "", "")
	setup := fs.String("setup", "", "setup script text")
	setupFile := fs.String("setup-file", "", "setup script file")
	secrets := fs.String("secrets", "", "comma-separated team secrets")
	vars := labels{}
	fs.Var(vars, "var", "KEY=value (repeatable)")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	cols := []string{"id", "name", "image", "size", "version", "is_default"}
	body := obj{}
	if *image != "" {
		body["image"] = *image
	}
	if *size != "" {
		body["size"] = *size
	}
	if *setup != "" && *setupFile != "" {
		return fmt.Errorf("--setup and --setup-file conflict: %w", errUsage)
	}
	if *setup != "" {
		body["setup_script"] = *setup
	}
	if *setupFile != "" {
		b, err := os.ReadFile(*setupFile)
		if err != nil {
			return err
		}
		body["setup_script"] = string(b)
	}
	if *secrets != "" {
		body["secrets"] = splitList(*secrets)
	}
	if len(vars) > 0 {
		body["env"] = vars
	}
	var out obj
	switch op {
	case "ls":
		l, err := listCall(a, "/v1/environments", "environments")
		if err != nil {
			return err
		}
		return a.emitList(l, cols...)
	case "new":
		if err := need(pos, 1, "NAME"); err != nil {
			return err
		}
		body["name"] = pos[0]
		if err := a.c.call("POST", "/v1/environments", body, &out); err != nil {
			return err
		}
		return a.show(out, cols...)
	}
	if err := need(pos, 1, "ENV"); err != nil {
		return err
	}
	p := "/v1/environments/" + url.PathEscape(pos[0])
	switch op {
	case "show":
		if err := a.c.call("GET", p, nil, &out); err != nil {
			return err
		}
		return a.emit(out, func() {
			b, _ := json.MarshalIndent(out, "", "  ")
			fmt.Fprintln(a.out, string(b))
		})
	case "set":
		if err := a.c.call("PUT", p, body, &out); err != nil {
			return err
		}
		return a.show(out, cols...)
	case "default":
		if err := a.c.call("PUT", p, obj{"is_default": true}, &out); err != nil {
			return err
		}
		return a.show(out, cols...)
	case "build":
		if err := a.c.call("POST", p+"/build", nil, &out); err != nil {
			return err
		}
		return a.okLine(out, "building "+s(out, "name")+"; check with: boat env show "+s(out, "name"))
	default:
		if err := a.c.call("DELETE", p, nil, &out); err != nil {
			return err
		}
		return a.okLine(out, "deleted "+pos[0])
	}
}

// ---- resize, info, resume ----

func cmdResize(a *app, args []string) error {
	fs := flag.NewFlagSet("resize", flag.ContinueOnError)
	size := fs.String("size", "", "")
	disk := fs.Int("disk", 0, "grow the disk to this many GiB")
	discard := fs.Bool("discard-memory", false, "drop the stop snapshot's memory, needed to change vCPU/RAM; the next start is a cold boot")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 2 && *size == "" {
		*size, pos = pos[1], pos[:1]
	}
	vm, err := oneVM(a, pos)
	if err != nil {
		return err
	}
	var out obj
	if err := a.c.call("POST", vmPath(s(vm, "id"), "resize"), obj{"size": *size, "disk_gib": *disk, "discard_memory": *discard}, &out); err != nil {
		return err
	}
	return a.emitVM(out)
}

func cmdInfo(a *app, args []string) error {
	vm, err := oneVM(a, args)
	if err != nil {
		return err
	}
	return a.emit(vm, func() {
		b, _ := json.MarshalIndent(vm, "", "  ")
		fmt.Fprintln(a.out, string(b))
	})
}

func cmdVMUsage(a *app, args []string) error {
	vm, err := oneVM(a, args)
	if err != nil {
		return err
	}
	var u obj
	if err := a.c.call("GET", vmPath(s(vm, "id"), "usage"), nil, &u); err != nil {
		return err
	}
	return a.show(u, "id", "vm_seconds")
}

// cmdResume is start, optionally on another size first.
func cmdResume(a *app, args []string) error {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	typ := fs.String("type", "", "resize first (drops a stop snapshot's memory)")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	vm, err := oneVM(a, pos)
	if err != nil {
		return err
	}
	if *typ != "" {
		if err := a.c.call("POST", vmPath(s(vm, "id"), "resize"), obj{"size": *typ, "discard_memory": true}, nil); err != nil {
			return err
		}
	}
	var out obj
	if err := a.c.call("POST", vmPath(s(vm, "id"), "start"), nil, &out); err != nil {
		return err
	}
	return a.emitVM(out)
}

// ---- snapshots: team-level ops, then per-VM ones ----

func cmdSnapshotsTeam(a *app, op string, args []string) error {
	fs := flag.NewFlagSet("snapshots", flag.ContinueOnError)
	name := fs.String("name", "", "")
	image := fs.String("image", "", "")
	size := fs.String("size", "", "")
	shareID := fs.String("share", "", "")
	code := fs.String("code", "", "")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	var out obj
	switch op {
	case "team":
		l, err := listCall(a, "/v1/snapshots", "snapshots")
		if err != nil {
			return err
		}
		return a.emitList(l, "id", "name", "vm_id", "created_at")
	case "shared":
		l, err := listCall(a, "/v1/shared-snapshots", "shares")
		if err != nil {
			return err
		}
		return a.emitList(l, "id", "snapshot_id", "expires", "uses")
	case "take":
		if (*shareID == "") == (*code == "") {
			return fmt.Errorf("give --share ID or --code CODE: %w", errUsage)
		}
		body := obj{}
		if *shareID != "" {
			body["share_id"] = *shareID
		} else {
			body["code"] = *code
		}
		if *name != "" {
			body["name"] = *name
		}
		if err := a.c.call("POST", "/v1/snapshots/import-share", body, &out); err != nil {
			return err
		}
		return a.show(out, "id", "name", "team")
	case "import":
		if err := need(pos, 1, "FILE"); err != nil {
			return err
		}
		f, err := os.Open(pos[0])
		if err != nil {
			return err
		}
		defer f.Close()
		q := url.Values{}
		for k, v := range map[string]string{"name": *name, "image": *image, "size": *size} {
			if v != "" {
				q.Set(k, v)
			}
		}
		resp, err := a.c.request("POST", "/v1/snapshots/import?"+q.Encode(), f, "application/octet-stream")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return err
		}
		return a.show(out, "id", "name", "team")
	default: // delete
		if err := need(pos, 1, "SNAPSHOT_ID"); err != nil {
			return err
		}
		if err := a.c.call("DELETE", "/v1/snapshots/"+url.PathEscape(pos[0]), nil, &out); err != nil {
			return err
		}
		return a.okLine(obj{"ok": true, "id": pos[0]}, "deleted snapshot "+pos[0])
	}
}

// vmSnapshotOp handles download/share/shares/unshare on <vm> <snapshot>.
func vmSnapshotOp(a *app, id, op string, args []string) error {
	fs := flag.NewFlagSet("snapshots", flag.ContinueOnError)
	output := fs.String("o", "", "output file")
	team := fs.String("team", "", "grant this team")
	ttl := fs.Duration("ttl", 0, "")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return fmt.Errorf("expected <snapshot-id>: %w", errUsage)
	}
	p := vmPath(id, "snapshots", url.PathEscape(pos[0]))
	var out obj
	switch op {
	case "download":
		dst := *output
		if dst == "" {
			dst = pos[0] + ".tar.zst"
		}
		resp, err := a.c.request("GET", p+"/download", nil, "")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) // may hold secrets
		if err != nil {
			return err
		}
		n, err := io.Copy(f, resp.Body)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(dst)
			return err
		}
		return a.okLine(obj{"ok": true, "path": dst, "bytes": n}, fmt.Sprintf("%s (%d bytes)", dst, n))
	case "share":
		body := obj{}
		if *team != "" {
			body["team_id"] = *team
		}
		if *ttl > 0 {
			body["ttl_s"] = int64(ttl.Seconds())
		}
		if err := a.c.call("POST", p+"/shares", body, &out); err != nil {
			return err
		}
		return a.emit(out, func() {
			fmt.Fprintf(a.out, "share %s\n", s(out, "id"))
			if c := s(out, "code"); c != "" {
				fmt.Fprintf(a.out, "code: %s (shown once; the snapshot's disk goes with it)\n", c)
				if u := s(out, "url"); u != "" {
					fmt.Fprintf(a.out, "link: %s\n", u)
				}
			}
		})
	case "shares":
		l, err := listCall(a, p+"/shares", "shares")
		if err != nil {
			return err
		}
		return a.emitList(l, "id", "grantee", "expires", "uses")
	default: // unshare
		if err := need(pos, 2, "<snapshot-id> <share-id>"); err != nil {
			return err
		}
		if err := a.c.call("DELETE", p+"/shares/"+url.PathEscape(pos[1]), nil, &out); err != nil {
			return err
		}
		return a.okLine(obj{"ok": true, "id": pos[1]}, "revoked "+pos[1])
	}
}

// ---- AI credentials proxy ----

func cmdAI(a *app, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: boat ai credentials|proxy|usage|block|unblock: %w", errUsage)
	}
	var out obj
	switch args[0] {
	case "credentials":
		op, pos := sub(args[1:], "ls", "ls", "set", "rm")
		switch op {
		case "ls":
			l, err := listCall(a, "/v1/ai/credentials", "credentials")
			if err != nil {
				return err
			}
			return a.emitList(l, "provider", "last4", "updated")
		case "set":
			if len(pos) < 1 || len(pos) > 2 {
				return fmt.Errorf("usage: boat ai credentials set PROVIDER [KEY|-]: %w", errUsage)
			}
			value := "-"
			if len(pos) == 2 {
				value = pos[1]
			}
			if value == "-" {
				b, err := io.ReadAll(io.LimitReader(a.in, 8192))
				if err != nil {
					return err
				}
				value = strings.TrimSpace(string(b))
			}
			if err := a.c.call("PUT", "/v1/ai/credentials/"+url.PathEscape(pos[0]), obj{"value": value}, &out); err != nil {
				return err
			}
			return a.show(out, "provider", "last4", "updated")
		default:
			if err := need(pos, 1, "PROVIDER"); err != nil {
				return err
			}
			if err := a.c.call("DELETE", "/v1/ai/credentials/"+url.PathEscape(pos[0]), nil, &out); err != nil {
				return err
			}
			return a.okLine(obj{"ok": true, "provider": pos[0]}, "removed "+pos[0])
		}
	case "proxy":
		if len(args) == 1 {
			if err := a.c.call("GET", "/v1/ai/proxy", nil, &out); err != nil {
				return err
			}
			return a.show(out, "enabled", "available")
		}
		if args[1] != "on" && args[1] != "off" {
			return fmt.Errorf("usage: boat ai proxy [on|off]: %w", errUsage)
		}
		if err := a.c.call("PUT", "/v1/ai/proxy", obj{"enabled": args[1] == "on"}, &out); err != nil {
			return err
		}
		return a.show(out, "enabled", "available")
	case "usage":
		p := "/v1/ai/usage"
		if len(args) == 2 {
			id, err := vmArg(a, args[1])
			if err != nil {
				return err
			}
			p = vmPath(id, "ai-usage")
		}
		l, err := listCall(a, p, "usage")
		if err != nil {
			return err
		}
		return a.emitList(l, "vm_id", "provider", "model", "requests", "input_tokens", "output_tokens")
	case "block", "unblock":
		if err := need(args[1:], 1, "<vm>"); err != nil {
			return err
		}
		id, err := vmArg(a, args[1])
		if err != nil {
			return err
		}
		if err := a.c.call("PUT", vmPath(id, "ai-access"), obj{"enabled": args[0] == "unblock"}, &out); err != nil {
			return err
		}
		return a.show(out, "id", "enabled")
	}
	return fmt.Errorf("unknown ai command %q: %w", args[0], errUsage)
}

// ---- telemetry ----

func cmdTelemetry(a *app, args []string) error {
	op, args := sub(args, "show", "show", "set", "unset")
	fs := flag.NewFlagSet("telemetry", flag.ContinueOnError)
	headers := labels{}
	fs.Var(headers, "H", "key=value (repeatable)")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	cols := []string{"configured", "endpoint", "header_names"}
	var out obj
	switch op {
	case "show":
		if err := a.c.call("GET", "/v1/telemetry", nil, &out); err != nil {
			return err
		}
		return a.show(out, cols...)
	case "set":
		if err := need(pos, 1, "ENDPOINT"); err != nil {
			return err
		}
		body := obj{"endpoint": pos[0]}
		if len(headers) > 0 {
			body["headers"] = headers
		}
		if err := a.c.call("PUT", "/v1/telemetry", body, &out); err != nil {
			return err
		}
		return a.show(out, cols...)
	default:
		if err := a.c.call("DELETE", "/v1/telemetry", nil, &out); err != nil {
			return err
		}
		return a.okLine(out, "telemetry removed")
	}
}
