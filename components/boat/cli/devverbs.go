package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// boat.dev's CLI verbs next to ours: extend, limits, snapshot, history and
// webhook take boat.dev's arguments. The VM-level ones use the native /v1
// API; webhook and limits use the server's /api/v1 (boat.dev's events and
// payloads; boat.dev's limits object).

func init() {
	for k, v := range map[string]func(*app, []string) error{
		"extend": cmdExtend, "limits": cmdLimits, "snapshot": cmdSnapshot, "history": cmdHistory, "webhook": cmdWebhook,
	} {
		commands[k] = v
	}
}

const maxTTLSeconds = 2592000

// cmdExtend: boat extend <vm> --hours N | --ttl SECS | --no-auto-stop set the
// remaining time from now (boat.dev's meaning); --by DURATION pushes the
// current deadline out instead.
func cmdExtend(a *app, args []string) error {
	fs := flag.NewFlagSet("extend", flag.ContinueOnError)
	hours := fs.Float64("hours", 0, "")
	ttl := fs.Int64("ttl", 0, "")
	never := fs.Bool("no-auto-stop", false, "")
	by := fs.Duration("by", 0, "")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	body := obj{}
	set := 0
	if *hours > 0 {
		body["ttl_s"], set = int64(*hours*3600), set+1
	}
	if *ttl > 0 {
		body["ttl_s"], set = *ttl, set+1
	}
	if *never {
		body["ttl_s"], set = 0, set+1
	}
	if *by > 0 {
		body["extend_s"], set = int64(by.Seconds()), set+1
	}
	if set != 1 {
		return fmt.Errorf("give one of --hours N, --ttl SECS, --no-auto-stop or --by DURATION: %w", errUsage)
	}
	if v, ok := body["ttl_s"].(int64); ok && v > maxTTLSeconds {
		return fmt.Errorf("at most 30 days (2592000 s): %w", errUsage)
	}
	vm, err := oneVM(a, pos)
	if err != nil {
		return err
	}
	var out obj
	if err := a.c.call("PATCH", vmPath(s(vm, "id")), body, &out); err != nil {
		return err
	}
	return a.emit(out, func() {
		if e := s(out, "expires_at"); e != "" {
			t, _ := time.Parse(time.RFC3339, e)
			fmt.Fprintf(a.out, "%s auto-stops at %s (in %s)\n", s(out, "id"), e, time.Until(t).Round(time.Minute))
		} else {
			fmt.Fprintf(a.out, "%s has no auto-stop\n", s(out, "id"))
		}
	})
}

func cmdLimits(a *app, args []string) error {
	fs := flag.NewFlagSet("limits", flag.ContinueOnError)
	fs.Bool("all", false, "accepted: every limit is shown")
	if _, _, err := parse(fs, args); err != nil {
		return err
	}
	var out obj
	if err := a.c.call("GET", "/api/v1/limits", nil, &out); err != nil {
		return err
	}
	delete(out, "ok")
	delete(out, "type")
	return a.emit(out, func() {
		limit := "the team quota is boatd's"
		if m, _ := out["maxActiveSandboxes"].(float64); m >= 0 {
			limit = fmt.Sprintf("at most %v", m)
		}
		fmt.Fprintf(a.out, "%v sandboxes active (%s); can start: %v\n", out["activeSandboxes"], limit, out["canStart"])
	})
}

// cmdSnapshot: boat snapshot <vm> NAME saves a named snapshot (a new one with
// a name in use replaces it); boat snapshot rm NAME removes it. Other forms
// are boat snapshots'.
func cmdSnapshot(a *app, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "latest", "tree", "pull", "delete", "share", "shares", "unshare":
			return cmdSnapshotMore(a, args[0], args[1:])
		}
	}
	if len(args) > 0 && (args[0] == "rm" || args[0] == "remove") {
		pos, _, err := parse(flag.NewFlagSet("snapshot", flag.ContinueOnError), args[1:])
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return fmt.Errorf("boat snapshot rm NAME: %w", errUsage)
		}
		var out obj
		if err := a.c.call("DELETE", "/api/v1/saved-snapshots/"+url.PathEscape(pos[0]), nil, &out); err != nil {
			return err
		}
		return a.show(obj{"name": pos[0], "status": "deleted"}, "name", "status")
	}
	pos, _, err := parse(flag.NewFlagSet("snapshot", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if len(pos) != 2 || strings.HasPrefix(pos[1], "-") {
		return cmdSnapshots(a, args)
	}
	vm, err := a.c.resolveVM(pos[0])
	if err != nil {
		return err
	}
	var out struct {
		Snapshot obj `json:"snapshot"`
	}
	if err := a.c.call("POST", "/api/v1/saved-snapshots", obj{"sandboxId": s(vm, "id"), "name": pos[1]}, &out); err != nil {
		return err
	}
	return a.show(out.Snapshot, "name", "snapshotId", "status", "createdAt")
}

// cmdHistory: boat history <vm> [command-id] [-f]: the commands of a VM, or
// one command's output.
func cmdHistory(a *app, args []string) error {
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	follow := fs.Bool("f", false, "")
	fs.BoolVar(follow, "follow", false, "")
	limit := fs.Int("limit", 20, "")
	mode := fs.String("mode", "", "commands: only what you ran (the only split kept here)")
	stderr := fs.Bool("stderr", false, "")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if *stderr {
		return errors.New("--stderr is not supported on self-hosted boat: a command keeps one combined log of stdout and stderr; omit --stderr")
	}
	if *mode != "" && *mode != "commands" {
		return errors.New("--mode " + *mode + " is not supported on self-hosted boat: the history keeps commands you ran (--mode commands), prompts and setup runs, with no process/sync/stream/detached split")
	}
	if len(pos) == 2 {
		rest := []string{"logs", pos[0], pos[1]}
		if *follow {
			rest = append(rest, "-f")
		}
		return cmdCommands(a, rest)
	}
	vm, err := oneVM(a, pos)
	if err != nil {
		return err
	}
	l, err := listCall(a, vmPath(s(vm, "id"), "command-history"), "commands")
	if err != nil {
		return err
	}
	// newest first, at most --limit, as boat.dev's
	out := make([]obj, 0, len(l))
	for i := len(l) - 1; i >= 0 && len(out) < *limit; i-- {
		if *mode == "" || s(l[i], "kind") == "command" {
			out = append(out, l[i])
		}
	}
	return a.emitList(out, "id", "kind", "state", "exit", "started_at", "cmd")
}

// cmdWebhook: boat webhook create URL [--name N] [--event ready ...] | list |
// rotate ID | remove ID, with boat.dev's events (sandbox. prefix optional; none = all six).
func cmdWebhook(a *app, args []string) error {
	op, args := sub(args, "list", "create", "list", "ls", "rotate", "remove", "rm")
	fs := flag.NewFlagSet("webhook", flag.ContinueOnError)
	name := fs.String("name", "", "")
	var events multi
	fs.Var(&events, "event", "")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	cols := []string{"id", "name", "url", "events"}
	switch op {
	case "list", "ls":
		l, err := listCall(a, "/api/v1/webhooks", "webhooks")
		if err != nil {
			return err
		}
		return a.emitList(l, cols...)
	case "create":
		if len(pos) != 1 {
			return fmt.Errorf("boat webhook create URL: %w", errUsage)
		}
		ev := []string{}
		for _, e := range events {
			for _, x := range strings.Split(e, ",") {
				if x = strings.TrimSpace(x); x != "" {
					ev = append(ev, "sandbox."+strings.TrimPrefix(x, "sandbox."))
				}
			}
		}
		if len(ev) == 0 {
			ev = []string{"sandbox.ready", "sandbox.hydrated", "sandbox.error", "sandbox.archived", "sandbox.degraded", "sandbox.recovered"}
		}
		body := obj{"url": pos[0], "events": ev}
		if *name != "" {
			body["name"] = *name
		}
		var out struct {
			Webhook obj    `json:"webhook"`
			Secret  string `json:"secret"`
		}
		if err := a.c.call("POST", "/api/v1/webhooks", body, &out); err != nil {
			return err
		}
		hook := obj{"secret": out.Secret}
		for k, v := range out.Webhook {
			hook[k] = v
		}
		hook["secret"] = out.Secret
		return a.show(hook, append(cols, "secret")...)
	case "rotate", "remove", "rm":
		if len(pos) != 1 {
			return fmt.Errorf("boat webhook %s ID: %w", op, errUsage)
		}
		p := "/api/v1/webhooks/" + url.PathEscape(pos[0])
		if op == "rotate" {
			var out struct {
				Webhook obj    `json:"webhook"`
				Secret  string `json:"secret"`
			}
			if err := a.c.call("POST", p+"/rotate", nil, &out); err != nil {
				return err
			}
			return a.show(obj{"id": pos[0], "secret": out.Secret}, "id", "secret")
		}
		if err := a.c.call("DELETE", p, nil, nil); err != nil {
			return err
		}
		return a.show(obj{"id": pos[0], "deleted": true}, "id", "deleted")
	}
	return errors.New("unreachable")
}

// multi is a repeatable string flag.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }
