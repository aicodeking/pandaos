package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

var commands map[string]func(*app, []string) error

func init() {
	commands = map[string]func(*app, []string) error{
		"whoami": cmdWhoami, "images": cmdImages, "usage": cmdUsage,
		"new": cmdNew, "list": cmdList, "ls": cmdList,
		"ssh": cmdSSH, "scp": cmdSCP, "exec": cmdExec, "prompt": cmdPrompt,
		"fork": cmdFork, "clone": cmdFork, "stop": cmdStop, "start": cmdResume, "rm": cmdRm,
		"desktop": cmdDesktop, "ports": cmdPorts, "snapshots": cmdSnapshots,
		"secrets": cmdSecrets, "keys": cmdKeys, "teams": cmdTeams, "webhooks": cmdWebhooks,
	}
}

var (
	vmCols   = []string{"id", "name", "size", "state", "image"}
	portCols = []string{"guest_port", "auth", "host_port", "hostname"}
)

func (a *app) emitVM(vm obj) error {
	return a.emitList([]obj{vm}, vmCols...)
}

func cmdWhoami(a *app, args []string) error {
	var me obj
	if err := a.c.call("GET", "/v1/me", nil, &me); err != nil {
		return err
	}
	return a.emit(me, func() {
		u, _ := me["user"].(map[string]any)
		t, _ := me["team"].(map[string]any)
		fmt.Fprintf(a.out, "%v (%v) in team %v as %v; scopes %v\n", u["name"], u["id"], t["name"], me["role"], me["scopes"])
	})
}

// listCall reads the named list from a response that may have other fields.
func listCall(a *app, path, key string) ([]obj, error) {
	var out map[string]json.RawMessage
	if err := a.c.call("GET", path, nil, &out); err != nil {
		return nil, err
	}
	var list []obj
	if raw, ok := out[key]; ok {
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, fmt.Errorf("%s: %s: %w", path, key, err)
		}
	}
	return list, nil
}

func cmdImages(a *app, args []string) error {
	l, err := listCall(a, "/v1/images", "images")
	if err != nil {
		return err
	}
	return a.emitList(l, "id", "default")
}

// cmdUsage: boat usage [--from X] [--to Y]. X and Y are YYYY-MM-DD or RFC3339; a date-only Y means the end of that UTC day (inclusive).
func cmdUsage(a *app, args []string) error {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	from := fs.String("from", "", "window start: YYYY-MM-DD or RFC3339")
	to := fs.String("to", "", "window end: YYYY-MM-DD (the whole UTC day, inclusive) or RFC3339")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("unexpected arguments %v: %w", pos, errUsage)
	}
	q := url.Values{}
	if *from != "" {
		q.Set("from", *from)
	}
	if *to != "" {
		q.Set("to", *to)
	}
	path := "/v1/usage"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var u obj
	if err := a.c.call("GET", path, nil, &u); err != nil {
		return err
	}
	return a.emit(u, func() {
		b, _ := json.MarshalIndent(u, "", "  ")
		fmt.Fprintln(a.out, string(b))
	})
}

type labels map[string]string

func (l labels) String() string { return "" }
func (l labels) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok {
		return errors.New("label must be key=value")
	}
	l[k] = val
	return nil
}

func cmdNew(a *app, args []string) error {
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	name := fs.String("name", "", "")
	size := fs.String("size", "", "")
	fs.StringVar(size, "type", "", "same as --size")
	image := fs.String("image", "", "")
	var ttl secsFlag
	fs.Var(&ttl, "ttl", "seconds (or a duration like 2h) until auto-stop")
	noAuto := fs.Bool("no-auto-stop", false, "never auto-stop")
	idle := fs.String("idle-stop", "", "auto-stop after this idle time (e.g. 30m), or never")
	snap := fs.String("snapshot", "", "create from a snapshot id")
	lab := labels{}
	fs.Var(lab, "label", "")
	secrets := fs.String("secrets", "", "comma-separated team secrets for this VM (default: the default-injected ones)")
	noSecrets := fs.Bool("no-secrets", false, "inject no team secrets")
	envName := fs.String("environment", "", "environment name")
	envv := labels{}
	fs.Var(envVarFlag{envv}, "e", "KEY=VALUE for the VM (repeatable); --environment NAME picks an environment")
	fs.Var(envVarFlag{envv}, "env", "same as -e")
	visibility := fs.String("visibility", "", "shared (every sandbox here is usable by its team)")
	fs.Bool("personal", false, "accepted: self-hosted boat has no org wallets")
	var extras newExtras
	extras.register(fs)
	var hit string
	refuse(fs, &hit, "this server always snapshots on stop", "no-snapshots")
	refuse(fs, &hit, "this server keeps command output and process samples", "no-monitoring")
	noEnv := fs.Bool("no-env", false, "ignore the team's default environment")
	from := fs.String("from", "", "a saved snapshot (id or name) or a share code bsh_...")
	waitSetup := fs.Bool("wait-setup", false, "follow the environment's setup script; fail if it fails")
	count := fs.Int("count", 1, fmt.Sprintf("create this many VMs (1-%d) in one request, all or none; --name x gives x-1 … x-N", maxBatch))
	failFast := fs.Bool("fail-fast", false, "fail at once with no_ready_machine when the host is full")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := refusedErr(hit); err != nil {
		return err
	}
	if err := ttlConflict(ttl, *noAuto); err != nil {
		return err
	}
	switch *visibility {
	case "", "shared":
	case "private":
		return errors.New("--visibility private is not supported on self-hosted boat: every sandbox is usable by its whole team")
	default:
		return fmt.Errorf("--visibility is private or shared: %w", errUsage)
	}
	if err := extras.check(); err != nil {
		return err
	}
	if len(pos) == 1 && *name == "" {
		*name = pos[0]
	} else if len(pos) > 0 {
		return fmt.Errorf("unexpected arguments %v: %w", pos, errUsage)
	}
	body := obj{"name": *name, "size": *size, "image": *image}
	// ttl_s only when asked: omitted, the server's default (1h, boat.dev's) applies;
	// --no-auto-stop, --ttl infinite and --ttl 0 send 0 (never auto-stop).
	if *noAuto {
		body["ttl_s"] = int64(0)
	} else if ttl.set {
		body["ttl_s"] = ttl.v
	}
	if *idle == "never" {
		body["idle_stop_s"] = -1
	} else if *idle != "" {
		d, err := time.ParseDuration(*idle)
		if err != nil || d <= 0 {
			return fmt.Errorf("--idle-stop wants a duration like 30m, or never: %w", errUsage)
		}
		body["idle_stop_s"] = int64(d.Seconds())
	}
	if *snap != "" {
		body["snapshot"] = *snap
	}
	if len(lab) > 0 {
		body["labels"] = lab
	}
	switch {
	case *noSecrets && *secrets != "":
		return fmt.Errorf("--secrets and --no-secrets conflict: %w", errUsage)
	case *noSecrets:
		body["secrets"] = []string{}
	case *secrets != "":
		body["secrets"] = splitList(*secrets)
	}
	if *envName != "" {
		body["environment"] = *envName
	}
	if *noEnv {
		if _, set := body["secrets"]; !set {
			body["secrets"] = []string{} // explicit empty list: no @default team secrets
		}
		body["no_env"] = true
	}
	if len(envv) > 0 {
		body["env"] = envv
	}
	switch {
	case *count < 1 || *count > maxBatch:
		return fmt.Errorf("--count must be 1-%d: %w", maxBatch, errUsage)
	case *count > 1 && *waitSetup:
		return fmt.Errorf("--count does not work with --wait-setup: %w", errUsage)
	case *count > 1 && extras.used():
		return fmt.Errorf("--count does not work with --host, --file or --setup-file: %w", errUsage)
	case *failFast && len(extras.hosts) > 0:
		return fmt.Errorf("--host does not work with --fail-fast: %w", errUsage)
	}
	if *failFast {
		body["fail_fast"] = true
	}
	if err := a.applyFrom(body, *from); err != nil {
		return err
	}
	if *count > 1 {
		return a.newBatch(body, *count)
	}
	var vm obj
	if err := a.c.call("POST", "/v1/vms", body, &vm); err != nil {
		return a.createStartFailure(err)
	}
	if ee := strings.TrimSpace(s(vm, "env_error")); ee != "" {
		return a.failEnv(vm, ee)
	}
	a.setCurrent(s(vm, "id"))
	if hosted := extras.apply(a, s(vm, "id")); len(hosted) > 0 {
		vm["hostedPorts"] = hosted
	}
	// --json: boat.dev's JSON Lines end with {"event":"ready","id":…}; this API
	// answers once the VM is up, so that one line (with every VM field) is all.
	if !a.json {
		if err := a.emitVM(vm); err != nil {
			return err
		}
	}
	if se := s(vm, "setup_error"); se != "" {
		fmt.Fprintln(a.errw, "boat: setup:", se)
	}
	if c := s(vm, "setup_cmd_id"); c != "" && *waitSetup {
		if err := a.followLogs(vmPath(s(vm, "id"), "commands", c, "logs")); err != nil {
			return err
		}
	}
	if a.json {
		return a.emit(readyEvent(vm), nil)
	}
	return nil
}

// failEnv reports a VM whose environment (secrets, BOAT_ID/token) never
// reached the guest: it is not ready, so no ready event and a non-zero exit.
func (a *app) failEnv(vm obj, msg string) error {
	id := s(vm, "id")
	if a.json {
		a.emit(obj{"event": "error", "id": id, "code": "env_error", "error": "environment delivery failed: " + msg}, nil)
		return exitError{1}
	}
	fmt.Fprintf(a.errw, "boat: %s was created but its environment was not delivered (%s); delete it with `boat delete %s`\n", id, msg, id)
	return exitError{1}
}

// failEnvUpdate reports a failed env UPDATE to an already-running VM: the API
// leaves it healthy with its previous environment, so nothing needs deleting.
func (a *app) failEnvUpdate(id, msg string) error {
	if a.json {
		a.emit(obj{"event": "error", "id": id, "code": "env_update_failed", "error": "environment update failed: " + msg}, nil)
		return exitError{1}
	}
	fmt.Fprintf(a.errw, "boat: environment update for %s failed (%s); the VM is still running with its previous environment\n", id, msg)
	return exitError{1}
}

// failEnvStart reports a no_env cold start that failed closed: the API stopped
// the VM and discarded its previous memory snapshot.
func (a *app) failEnvStart(id, msg string) error {
	if a.json {
		a.emit(obj{"event": "error", "id": id, "code": "env_start_failed", "state": "stopped", "error": "start failed: " + msg + " (VM stopped; previous session memory discarded)"}, nil)
		return exitError{1}
	}
	fmt.Fprintf(a.errw, "boat: start of %s failed (%s); the VM was stopped (no_env start failed closed; previous session memory discarded)\n", id, msg)
	return exitError{1}
}

// forkStartFailure reports a no_env fork/clone whose fresh boot failed closed:
// the child exists but is stopped. Other errors pass through unchanged.
func (a *app) forkStartFailure(err error) error {
	var ae *apiError
	if !errors.As(err, &ae) || ae.Code != "env_start_failed" {
		return err
	}
	child := s(obj(ae.VM), "id")
	if child == "" {
		return err
	}
	if a.json {
		a.emit(obj{"event": "error", "id": child, "child": child, "code": "env_start_failed", "state": "stopped", "error": "no_env fresh boot failed: " + ae.Message + " (child " + child + " was stopped)"}, nil)
		return exitError{1}
	}
	fmt.Fprintf(a.errw, "boat: child %s was stopped (no_env fresh boot failed: %s)\n", child, ae.Message)
	return exitError{1}
}

// createStartFailure reports a create (new/run/batch) whose fresh boot failed
// closed: the API kept the VM(s) it names in vm/vms, stopped or still running.
// Other errors pass through unchanged.
func (a *app) createStartFailure(err error) error {
	var ae *apiError
	if !errors.As(err, &ae) || ae.Code != "env_start_failed" {
		return err
	}
	vms := make([]map[string]any, 0, len(ae.VMs)+1)
	seen := map[string]bool{}
	for _, v := range append([]map[string]any{ae.VM}, ae.VMs...) {
		if v == nil {
			continue
		}
		if id := s(obj(v), "id"); id != "" {
			if seen[id] {
				continue
			}
			seen[id] = true
		}
		vms = append(vms, v)
	}
	if len(vms) == 0 {
		return err
	}
	type entry struct{ id, state string }
	var es []entry
	evs := make([]any, 0, len(vms))
	for _, v := range vms {
		st := s(obj(v), "state")
		if st == "" {
			st = "unknown"
		}
		es = append(es, entry{s(obj(v), "id"), st})
		evs = append(evs, obj{"id": s(obj(v), "id"), "state": st})
	}
	if a.json {
		a.emit(obj{"event": "error", "id": es[0].id, "code": "env_start_failed", "status": ae.Status, "state": es[0].state, "vms": evs, "error": "fresh boot env push failed: " + ae.Message}, nil)
		return exitError{1}
	}
	for _, e := range es {
		fmt.Fprintf(a.errw, "boat: %s %s (fresh boot env push failed: %s)\n", e.id, e.state, ae.Message)
	}
	return exitError{1}
}

// readyEvent is boat.dev's last `boat new --json` line: the VM with "event".
func readyEvent(vm obj) obj {
	ev := obj{"event": "ready"}
	for k, v := range vm {
		ev[k] = v
	}
	return ev
}

func cmdList(a *app, args []string) error {
	l, err := listCall(a, "/v1/vms", "vms")
	if err != nil {
		return err
	}
	return a.emitList(markEnvFailed(l, a.json), vmCols...)
}

// markEnvFailed annotates the state column of VMs whose environment was not
// delivered ("running (env failed)"). JSON output is returned untouched.
func markEnvFailed(l []obj, json bool) []obj {
	if json {
		return l
	}
	out := make([]obj, len(l))
	for i, vm := range l {
		out[i] = vm
		if strings.TrimSpace(s(vm, "env_error")) == "" {
			continue
		}
		cp := make(obj, len(vm))
		for k, v := range vm {
			cp[k] = v
		}
		cp["state"] = s(vm, "state") + " (env failed)"
		out[i] = cp
	}
	return out
}

func oneVM(a *app, args []string) (obj, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("expected <vm>: %w", errUsage)
	}
	return a.c.resolveVM(args[0])
}

// cmdStop stops (snapshots) a VM; --force kills it without a snapshot.
func cmdStop(a *app, args []string) error {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	force := fs.Bool("force", false, "kill at once, no snapshot: the next start is a cold boot and loses unsynced writes")
	status := fs.String("status", "", "read this stop operation (stop_...)")
	fs.Bool("yes", false, "accepted: stop never asks")
	fs.Bool("y", false, "same as --yes")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	vm, err := oneVM(a, pos)
	if err != nil {
		return err
	}
	if *status != "" {
		return cmdStopStatus(a, s(vm, "id"), *status)
	}
	var body any
	if *force {
		body = obj{"force": true}
	}
	var out obj
	if err := a.c.call("POST", vmPath(s(vm, "id"), "stop"), body, &out); err != nil {
		return err
	}
	return a.emitVM(out)
}

func cmdRm(a *app, args []string) error {
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "confirm deletion (required with --json or without a terminal)")
	fs.BoolVar(yes, "y", false, "same as --yes")
	pos, after, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(after) > 0 {
		return fmt.Errorf("unexpected arguments after --: %w", errUsage)
	}
	if err := need(pos, 1, "<vm>"); err != nil {
		return err
	}
	if err := a.requireConfirmFlag(*yes); err != nil {
		return err
	}
	vm, err := oneVM(a, pos)
	if err != nil {
		return err
	}
	if err := a.confirmDestroy(*yes, s(vm, "id")); err != nil {
		return err
	}
	if err := a.c.call("DELETE", vmPath(s(vm, "id")), nil, nil); err != nil {
		return err
	}
	return a.emit(obj{"ok": true, "id": s(vm, "id")}, func() { fmt.Fprintln(a.out, "deleted", s(vm, "id")) })
}

func (a *app) requireConfirmFlag(yes bool) error {
	if !yes && (a.json || a.isTerminal == nil || !a.isTerminal()) {
		return errors.New("deletion requires --yes (-y) with --json or without a terminal")
	}
	return nil
}

func (a *app) confirmDestroy(yes bool, target string) error {
	if yes {
		return nil
	}
	if err := a.requireConfirmFlag(yes); err != nil {
		return err
	}
	fmt.Fprintf(a.errw, "Permanently delete %s? [y/N] ", target)
	answer, err := bufio.NewReader(io.LimitReader(a.in, 128)).ReadString('\n')
	if err != nil {
		return fmt.Errorf("deletion not confirmed (use --yes to confirm): %w", err)
	}
	answer = strings.TrimSpace(answer)
	if !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
		return errors.New("deletion cancelled")
	}
	return nil
}

func cmdFork(a *app, args []string) error {
	fs := flag.NewFlagSet("fork", flag.ContinueOnError)
	name := fs.String("name", "", "")
	typ := fs.String("type", "", "resize fork to this size (cold boot)")
	fs.StringVar(typ, "size", "", "same as --type")
	failFast := fs.Bool("fail-fast", false, "fail at once with no_ready_machine when the host is full")
	var ttl secsFlag
	fs.Var(&ttl, "ttl", "seconds until the fork auto-stops (0 or infinite = never)")
	never := fs.Bool("no-auto-stop", false, "the fork never auto-stops")
	noEnv := fs.Bool("no-env", false, "do not inject team secrets or inherited environment")
	envv := labels{}
	envName := fs.String("environment", "", "apply this environment's variables and secrets to the fork (refused on a no_env VM: no_env is sticky)")
	fs.Var(envVarFlag{envv}, "e", "KEY=VALUE for the fork (repeatable); --environment NAME picks an environment")
	fs.Var(envVarFlag{envv}, "env", "same as -e")
	fs.Bool("wait", false, "accepted: fork answers once the fork is up")
	var hit string
	refuse(fs, &hit, "this server always snapshots on stop", "no-snapshots")
	refuse(fs, &hit, "this server keeps command output and process samples", "no-monitoring")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := refusedErr(hit); err != nil {
		return err
	}
	if err := ttlConflict(ttl, *never); err != nil {
		return err
	}
	if *noEnv && *envName != "" {
		return fmt.Errorf("--no-env and --environment conflict: %w", errUsage)
	}
	vm, err := oneVM(a, pos)
	if err != nil {
		return err
	}
	if *typ != "" {
		// Before the fork exists: a no_env parent is sticky, and an unknown
		// environment or size must fail with no state-changing call.
		parentNoEnv, _ := vm["no_env_run"].(bool)
		if err := refuseStickyEnv(s(vm, "id"), "fork", *envName, parentNoEnv); err != nil {
			return err
		}
		if err := a.checkEnvironment(*envName); err != nil {
			return err
		}
		if err := a.checkSize(*typ); err != nil {
			return err
		}
	}
	body := obj{"name": *name}
	if *failFast {
		body["fail_fast"] = true
	}
	if len(envv) > 0 {
		body["env"] = envv
	}
	if *noEnv {
		body["no_env"] = true
	}
	if *envName != "" {
		body["environment"] = *envName
	}
	if *never {
		body["ttl_s"] = int64(0)
	} else if ttl.set {
		body["ttl_s"] = ttl.v
	}
	var out obj
	if err := a.c.call("POST", vmPath(s(vm, "id"), "fork"), body, &out); err != nil {
		return a.forkStartFailure(stickyErr(err))
	}
	forkID := s(out, "id")
	if *typ != "" && s(out, "size") != *typ {
		retype := func() error {
			if s(out, "state") != "stopped" {
				if err := a.c.call("POST", vmPath(forkID, "stop"), nil, nil); err != nil {
					return err
				}
			}
			if err := a.c.call("POST", vmPath(forkID, "resize"), obj{"size": *typ, "discard_memory": true}, nil); err != nil {
				return err
			}
			startBody := obj{}
			if *failFast {
				startBody["fail_fast"] = true
			}
			if len(envv) > 0 {
				startBody["env"] = envv
			}
			if *noEnv {
				startBody["no_env"] = true
			}
			if *envName != "" {
				// A named environment ends the no_env run; never force no_env.
				startBody["environment"] = *envName
			} else if runNoEnv, _ := out["no_env_run"].(bool); runNoEnv {
				startBody["no_env"] = true // the fork continues a run that held no team secrets
			} else if parentNoEnv, _ := vm["no_env_run"].(bool); parentNoEnv {
				startBody["no_env"] = true
			}
			if *never {
				startBody["ttl_s"] = int64(0)
			} else if ttl.set {
				startBody["ttl_s"] = ttl.v
			}
			var send any
			if len(startBody) > 0 {
				send = startBody
			}
			return stickyErr(a.c.call("POST", vmPath(forkID, "start"), send, &out))
		}
		if err := retype(); err != nil {
			// Every failure in the --type phase deletes the child, like compat
			// fork-with-type; only the fork call itself keeps a stopped child.
			if derr := a.c.call("DELETE", vmPath(forkID), nil, nil); derr != nil {
				return fmt.Errorf("%w (cleanup delete %s also failed: %v)", err, forkID, derr)
			}
			return fmt.Errorf("%w (child %s was deleted)", err, forkID)
		}
	}
	if ee := strings.TrimSpace(s(out, "env_error")); ee != "" {
		return a.failEnv(out, ee)
	}
	a.setCurrent(forkID)
	return a.emitVM(out)
}

// ---- exec / prompt ----

func cmdExec(a *app, args []string) error {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	timeout := fs.Int("timeout", 0, "seconds")
	cwd := fs.String("cwd", "", "")
	detach := fs.Bool("detach", false, "run in the background; prints the command id")
	fs.Bool("stream", false, "accepted: output always streams as the command writes it")
	status := fs.String("status", "", "status and output of a --detach command")
	attach := fs.String("attach", "", "follow a --detach command's output to its end; exits with its code")
	kill := fs.String("kill", "", "stop a --detach command")
	pos, cmd, err := parse(fs, args)
	if err != nil {
		return err
	}
	visited := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })
	var modeFlags []string
	var op, pid string
	if visited["status"] {
		if *status == "" {
			return fmt.Errorf("--status requires a command id: %w", errUsage)
		}
		modeFlags = append(modeFlags, "--status")
		op, pid = "status", *status
	}
	if visited["attach"] {
		if *attach == "" {
			return fmt.Errorf("--attach requires a command id: %w", errUsage)
		}
		modeFlags = append(modeFlags, "--attach")
		op, pid = "logs", *attach
	}
	if visited["kill"] {
		if *kill == "" {
			return fmt.Errorf("--kill requires a command id: %w", errUsage)
		}
		modeFlags = append(modeFlags, "--kill")
		op, pid = "stop", *kill
	}
	if len(modeFlags) > 1 {
		return fmt.Errorf("%s cannot be combined: %w", strings.Join(modeFlags, " and "), errUsage)
	}
	if len(modeFlags) == 1 {
		if *detach {
			return fmt.Errorf("%s cannot be combined with --detach: %w", modeFlags[0], errUsage)
		}
		if len(pos) > 1 {
			return fmt.Errorf("unexpected arguments %v with %s: %w", pos[1:], modeFlags[0], errUsage)
		}
		if len(cmd) > 0 {
			return fmt.Errorf("unexpected command %v with %s: %w", cmd, modeFlags[0], errUsage)
		}
		rest := []string{op, orStr(firstOr(pos, ""), "current"), pid}
		if op == "logs" {
			rest = append(rest, "-f")
		}
		if op == "status" && !a.json {
			if err := cmdCommands(a, rest); err != nil {
				return err
			}
			return cmdCommands(a, []string{"logs", rest[1], pid})
		}
		return cmdCommands(a, rest)
	}
	if len(pos) == 2 && len(cmd) == 0 { // boat exec vm "cmd string"
		cmd, pos = []string{"bash", "-lc", pos[1]}, pos[:1]
	} else if len(pos) > 2 && len(cmd) == 0 {
		return fmt.Errorf("unexpected arguments %v (use -- before the command): %w", pos[1:], errUsage)
	}
	if len(cmd) == 0 {
		return fmt.Errorf("usage: boat exec <vm> -- cmd: %w", errUsage)
	}
	vm, err := oneVM(a, orCurrent(pos, 1))
	if err != nil {
		return err
	}
	if *detach {
		var out obj
		if err := a.c.call("POST", vmPath(s(vm, "id"), "commands"), obj{"cmd": cmd, "cwd": *cwd}, &out); err != nil {
			return err
		}
		return a.show(out, "id", "state", "cmd")
	}
	return a.stream(vmPath(s(vm, "id"), "exec"), obj{"cmd": cmd, "timeout_s": *timeout, "cwd": *cwd})
}

func cmdPrompt(a *app, args []string) error {
	fs := flag.NewFlagSet("prompt", flag.ContinueOnError)
	agent := fs.String("agent", "claude", "")
	fs.StringVar(agent, "provider", "claude", "same as --agent")
	model := fs.String("model", "", "")
	effort := fs.String("reasoning-effort", "", "accepted; each harness picks its own thinking level here")
	fast := fs.Bool("fast", false, "accepted; there is no fast mode here")
	var attach multi
	fs.Var(&attach, "attach", "a file for the agent, saved under ~/attachments (repeatable)")
	fs.Var(&attach, "image", "same as --attach")
	cwd := fs.String("cwd", "", "")
	timeout := fs.Int("timeout", 0, "seconds")
	var turn promptFlags
	turn.register(fs)
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	pos = orCurrent(pos, 2)
	if err := need(pos, 2, `<vm> "<prompt>"`); err != nil {
		return err
	}
	text := pos[1]
	if text == "-" {
		b, err := io.ReadAll(a.in)
		if err != nil {
			return err
		}
		text = string(b)
	}
	vm, err := a.c.resolveVM(pos[0])
	if err != nil {
		return err
	}
	if *effort != "" || *fast {
		fmt.Fprintln(a.errw, "boat: note: --reasoning-effort and --fast have no effect on self-hosted boat")
	}
	if text, err = a.attach(s(vm, "id"), attach, text); err != nil {
		return err
	}
	if turn.used() {
		body := obj{"prompt": text, "model": *model, "cwd": *cwd}
		// Only an explicit --agent: a conversation resumes with its own agent.
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "agent" || f.Name == "provider" {
				body["agent"] = *agent
			}
		})
		return a.startTurn(s(vm, "id"), body, &turn)
	}
	return a.stream(vmPath(s(vm, "id"), "prompt"), obj{"prompt": text, "agent": *agent, "model": *model, "cwd": *cwd, "timeout_s": *timeout})
}

// stream relays exec JSON lines. --json passes them through verbatim;
// otherwise output is decoded to stdout/stderr. Returns the remote exit code.
func (a *app) stream(path string, body obj) error {
	b, _ := json.Marshal(body)
	resp, err := a.c.requestCtx(a.context(), "POST", path, bytes.NewReader(b), "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	exit := -1
	for sc.Scan() {
		line := sc.Bytes()
		var ev struct {
			Stream   string `json:"stream"`
			DataB64  string `json:"data_b64"`
			Exit     *int   `json:"exit"`
			TimedOut bool   `json:"timed_out"`
			Error    any    `json:"error"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		if a.json {
			a.out.Write(append(bytes.TrimSpace(line), '\n'))
		}
		if ev.Exit != nil {
			exit = *ev.Exit
		}
		if ev.TimedOut {
			exit = 124
		}
		if a.json {
			continue
		}
		data, _ := base64.StdEncoding.DecodeString(ev.DataB64)
		if ev.Stream == "stderr" {
			a.errw.Write(data)
		} else {
			a.out.Write(data)
		}
		if ev.Error != nil {
			fmt.Fprintln(a.errw, "boat: remote error:", ev.Error)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if exit < 0 {
		return errors.New("stream ended without exit status")
	}
	if exit != 0 {
		return exitError{exit}
	}
	return nil
}

// ---- ssh / scp / desktop ----

func (a *app) publicKey(path string) (string, error) {
	cands := []string{path}
	if path == "" {
		home := homeDir(a.getenv)
		cands = nil
		for _, n := range []string{"id_ed25519.pub", "id_ecdsa.pub", "id_rsa.pub"} {
			cands = append(cands, filepath.Join(home, ".ssh", n))
		}
	}
	for _, p := range cands {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(b)), nil
		}
	}
	return "", errors.New("no ssh public key found (~/.ssh/id_ed25519.pub); create one with ssh-keygen or pass --identity")
}

func cmdSSH(a *app, args []string) error {
	fs := flag.NewFlagSet("ssh", flag.ContinueOnError)
	ident := fs.String("identity", "", "public key file")
	tty := fs.Bool("t", false, "give the command a terminal (tmux, read -s)")
	fs.BoolVar(tty, "tty", false, "same as -t")
	pos, extra, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 2 && len(extra) == 0 { // boat ssh <vm> "cmd": boat.dev's one-command form
		pos, extra = pos[:1], []string{pos[1]}
	}
	vm, err := oneVM(a, orCurrent(pos, 1))
	if err != nil {
		return err
	}
	info, err := a.sshTarget(s(vm, "id"), *ident)
	if err != nil {
		return err
	}
	var opts []string
	if *tty {
		opts = []string{"-t"}
	}
	base, err := sshArgv(s(vm, "id"), info, opts...)
	if err != nil {
		return err
	}
	argv := append(base, extra...)
	if a.json {
		info["argv"] = argv
		return a.emit(info, nil)
	}
	return a.execProg(argv)
}

// sshJumpArgs turns user@host[:port] (including bracketed or bare IPv6 literals)
// into "-p <port> -- <user>@<bare-host>" so no shell glob characters reach ProxyCommand.
func sshJumpArgs(j string) (string, error) {
	user, host, port, ok := parseSSHJump(j)
	if !ok {
		return "", fmt.Errorf("server returned an invalid ssh jump host %q", j)
	}
	return "-p " + port + " -- " + user + "@" + host, nil
}

func cmdDesktop(a *app, args []string) error {
	fs := flag.NewFlagSet("desktop", flag.ContinueOnError)
	noOpen := fs.Bool("no-open", false, "")
	var hit string
	refuse(fs, &hit, "browser opens the VM's desktop; start Chrome there with --user-data-dir", "profile=")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := refusedErr(hit); err != nil {
		return err
	}
	vm, err := oneVM(a, pos)
	if err != nil {
		return err
	}
	var d obj
	if err := a.c.call("GET", vmPath(s(vm, "id"), "desktop"), nil, &d); err != nil {
		return err
	}
	raw := s(d, "url")
	if err := validateOpenURL(raw); err != nil {
		return err
	}
	if a.json || *noOpen {
		return a.emit(d, func() { fmt.Fprintln(a.out, raw) })
	}
	fmt.Fprintln(a.out, raw)
	return a.open(raw)
}

// ---- ports / snapshots ----

func cmdPorts(a *app, args []string) error {
	op, args := sub(args, "ls", "ls", "add", "rm", "url", "revoke")
	fs := flag.NewFlagSet("ports", flag.ContinueOnError)
	public := fs.Bool("public", false, "open route + raw TCP port (same as --auth public)")
	auth := fs.String("auth", "", "private (default: gateway route needs a token from `boat ports url`) or public")
	ttl := fs.Duration("ttl", 0, "lifetime of a private port URL (default: server's, 1h)")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	want := 2
	if op == "ls" || op == "revoke" {
		want = 1
	}
	if err := need(pos, want, "<vm> [port]"); err != nil {
		return err
	}
	vm, err := a.c.resolveVM(pos[0])
	if err != nil {
		return err
	}
	id := s(vm, "id")
	switch op {
	case "ls":
		l, err := listCall(a, vmPath(id, "ports"), "ports")
		if err != nil {
			return err
		}
		return a.emitList(l, portCols...)
	case "add":
		port, err := strconv.Atoi(pos[1])
		if err != nil {
			return fmt.Errorf("port must be a number: %w", errUsage)
		}
		mode := *auth
		if *public {
			if mode == "private" {
				return fmt.Errorf("--public conflicts with --auth private: %w", errUsage)
			}
			mode = "public"
		}
		if mode == "" {
			mode = "private"
		}
		var out obj
		if err := a.c.call("POST", vmPath(id, "ports"), obj{"port": port, "auth": mode}, &out); err != nil {
			return err
		}
		return a.emitList([]obj{out}, portCols...)
	case "url":
		path := vmPath(id, "ports", url.PathEscape(pos[1]), "url")
		if *ttl > 0 {
			path += "?ttl_s=" + strconv.Itoa(int(ttl.Seconds()))
		}
		var out obj
		if err := a.c.call("GET", path, nil, &out); err != nil {
			return err
		}
		return a.emit(out, func() {
			fmt.Fprintln(a.out, s(out, "url"))
			if t := s(out, "token"); t != "" {
				fmt.Fprintf(a.errw, "private route, valid until %s; for scripts: curl -H 'X-Boat-Route-Token: %s' …\n", s(out, "expires_at"), t)
			}
		})
	case "revoke":
		var out obj
		if err := a.c.call("POST", vmPath(id, "port-tokens", "revoke"), nil, &out); err != nil {
			return err
		}
		return a.emit(out, func() {
			if g := numStr(out, "generation"); g != "" {
				fmt.Fprintf(a.out, "revoked port tokens for %s (generation %s)\n", id, g)
				return
			}
			fmt.Fprintf(a.out, "revoked port tokens for %s\n", id)
		})
	default:
		if err := a.c.call("DELETE", vmPath(id, "ports", url.PathEscape(pos[1])), nil, nil); err != nil {
			return err
		}
		return a.emit(obj{"ok": true, "port": pos[1]}, func() { fmt.Fprintln(a.out, "removed port", pos[1]) })
	}
}

func cmdSnapshots(a *app, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return cmdSnapshotsTeam(a, "team", args)
	}
	switch args[0] {
	case "team", "shared", "import", "take", "delete":
		return cmdSnapshotsTeam(a, args[0], args[1:])
	}
	vm, err := a.c.resolveVM(args[0])
	if err != nil {
		return err
	}
	id := s(vm, "id")
	if len(args) > 1 {
		switch args[1] {
		case "download", "share", "shares", "unshare":
			return vmSnapshotOp(a, id, args[1], args[2:])
		}
	}
	op, args := sub(args[1:], "ls", "ls", "create", "rm")
	fs := flag.NewFlagSet("snapshots", flag.ContinueOnError)
	name := fs.String("name", "", "")
	limit := fs.Int("limit", 0, "at most this many, newest first")
	fs.Bool("all", false, "accepted: every snapshot is listed")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	switch op {
	case "ls":
		l, err := listCall(a, vmPath(id, "snapshots"), "snapshots")
		if err != nil {
			return err
		}
		if *limit > 0 && len(l) > *limit {
			l = l[len(l)-*limit:]
		}
		return a.emitSnapshots(l)
	case "create":
		var out obj
		if err := a.c.call("POST", vmPath(id, "snapshots"), obj{"name": *name}, &out); err != nil {
			return err
		}
		return a.emitSnapshots([]obj{out})
	default:
		if err := need(pos, 1, "<snapshot-id>"); err != nil {
			return err
		}
		if err := a.c.call("DELETE", vmPath(id, "snapshots", url.PathEscape(pos[0])), nil, nil); err != nil {
			return err
		}
		return a.emit(obj{"ok": true, "id": pos[0]}, func() { fmt.Fprintln(a.out, "deleted snapshot", pos[0]) })
	}
}

func numStr(m obj, k string) string {
	switch v := m[k].(type) {
	case float64:
		if float64(int64(v)) == v {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	default:
		return s(m, k)
	}
}

func snapshotSize(it obj) string {
	if sb := numStr(it, "size_bytes"); sb != "" {
		return sb
	}
	disk, mem := numStr(it, "disk_gib"), numStr(it, "mem_mib")
	switch {
	case disk != "" && mem != "":
		return disk + " GiB / " + mem + " MiB"
	case disk != "":
		return disk + " GiB"
	case mem != "":
		return mem + " MiB"
	default:
		return s(it, "size")
	}
}

func (a *app) emitSnapshots(items []obj) error {
	if a.json {
		return a.emitList(items)
	}
	rows := make([]obj, len(items))
	for i, it := range items {
		rows[i] = obj{
			"id":         s(it, "id"),
			"name":       s(it, "name"),
			"created_at": orStr(s(it, "created_at"), s(it, "created")),
			"size":       snapshotSize(it),
		}
	}
	return a.emitList(rows, "id", "name", "created_at", "size")
}

// ---- secrets / keys / teams / webhooks ----

// boat secrets set marks the secret default-injected (every VM created without
// --secrets gets it), as boat.dev's CLI secrets are; --no-inject stores it for
// VMs and environments that select it by name. rotate keeps the mark. The API's
// own default (PUT /v1/secrets without inject) is not injected.
func cmdSecrets(a *app, args []string) error {
	noInject := slices.Contains(args, "--no-inject")
	args = slices.DeleteFunc(slices.Clone(args), func(s string) bool { return s == "--no-inject" })
	op, pos := sub(args, "ls", "ls", "set", "rotate", "rm")
	if noInject && op != "set" {
		return fmt.Errorf("--no-inject is for boat secrets set: %w", errUsage)
	}
	switch op {
	case "ls":
		l, err := listCall(a, "/v1/secrets", "secrets")
		if err != nil {
			return err
		}
		return a.emitList(l, "name", "inject", "updated")
	case "set", "rotate":
		if len(pos) < 1 || len(pos) > 2 {
			return fmt.Errorf("usage: boat secrets %s NAME [VALUE|-]: %w", op, errUsage)
		}
		if op == "rotate" && !a.secretExists(pos[0]) {
			return fmt.Errorf("secret %s does not exist; use boat secrets set", pos[0])
		}
		var value string
		if len(pos) == 2 && pos[1] != "-" {
			value = pos[1]
		} else {
			b, err := io.ReadAll(io.LimitReader(a.in, 64<<10+1))
			if err != nil {
				return err
			}
			value = strings.TrimRight(string(b), "\r\n")
		}
		body := obj{"value": value}
		if op == "set" {
			body["inject"] = !noInject
		}
		var out obj
		if err := a.c.call("PUT", "/v1/secrets/"+url.PathEscape(pos[0]), body, &out); err != nil {
			return err
		}
		return a.emit(out, func() {
			fmt.Fprintln(a.out, "set", pos[0])
			if out["rotated"] == true {
				fmt.Fprintln(a.errw, "note:", s(out, "note"))
				fmt.Fprintln(a.errw, "hint: restart VMs that need the new value (boat stop VM && boat start VM); VMs created with --secrets without it are unaffected")
			}
		})
	default:
		if err := need(pos, 1, "NAME"); err != nil {
			return err
		}
		if err := a.c.call("DELETE", "/v1/secrets/"+url.PathEscape(pos[0]), nil, nil); err != nil {
			return err
		}
		return a.emit(obj{"ok": true, "name": pos[0]}, func() { fmt.Fprintln(a.out, "removed", pos[0]) })
	}
}

// omitNil drops empty lists, which the API's schema does not allow as null.
func omitNil(o obj) obj {
	out := obj{}
	for k, v := range o {
		if l, ok := v.([]string); ok && l == nil {
			continue
		}
		out[k] = v
	}
	return out
}

func overlapBody(d time.Duration) obj {
	if d < 0 {
		return obj{}
	}
	return obj{"overlap_s": int64(d.Seconds())}
}

const maxKeyTTLS = 365 * 24 * 3600

// parseKeyTTL parses a key lifetime like 24h, 7d, 90d, 365d, integer seconds,
// or "never" into expires_in_s (0 means omit / never expires).
func parseKeyTTL(raw string) (int64, error) {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" || v == "never" {
		return 0, nil
	}
	bad := fmt.Errorf("ttl must be a duration such as 24h, 7d, 90d, 365d or seconds (up to 365d), or never: %w", errUsage)
	if daysStr, ok := strings.CutSuffix(v, "d"); ok {
		n, err := strconv.ParseInt(daysStr, 10, 64)
		if err != nil || n <= 0 || n > maxKeyTTLS/86400 {
			return 0, bad
		}
		return n * 86400, nil
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n <= 0 || n > maxKeyTTLS {
			return 0, bad
		}
		return n, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 || d%time.Second != 0 {
		return 0, bad
	}
	secs := int64(d / time.Second)
	if secs <= 0 || secs > maxKeyTTLS {
		return 0, bad
	}
	return secs, nil
}

func cmdKeys(a *app, args []string) error {
	op, args := sub(args, "ls", "ls", "create", "rm", "rotate", "usage")
	fs := flag.NewFlagSet("keys", flag.ContinueOnError)
	overlap := fs.Duration("overlap", -1, "how long the old secret keeps working (default 24h; 0 = revoke now)")
	name := fs.String("name", "", "")
	scopes := fs.String("scopes", "", "")
	team := fs.String("team", "", "")
	ttl := fs.String("ttl", "", "key lifetime: 24h|7d|90d|365d|never (up to 365d)")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	switch op {
	case "ls":
		l, err := listCall(a, "/v1/keys", "keys")
		if err != nil {
			return err
		}
		return a.emitList(l, "id", "name", "user_id", "scopes", "created")
	case "rotate":
		if err := need(pos, 1, "KEY_ID"); err != nil {
			return err
		}
		var out obj
		if err := a.c.call("POST", "/v1/keys/"+url.PathEscape(pos[0])+"/rotate", overlapBody(*overlap), &out); err != nil {
			return err
		}
		return a.emit(out, func() {
			fmt.Fprintf(a.out, "%s\n(new secret for key %s; shown once)\n", s(out, "token"), s(out, "id"))
			if u := s(out, "prev_until"); u != "" {
				fmt.Fprintf(a.errw, "the old secret works until %s\n", u)
			}
		})
	case "usage":
		if err := need(pos, 1, "KEY_ID"); err != nil {
			return err
		}
		var out obj
		if err := a.c.call("GET", "/v1/keys/"+url.PathEscape(pos[0])+"/usage", nil, &out); err != nil {
			return err
		}
		return a.show(out, "id", "requests", "last_used")
	case "create":
		expiresInS, err := parseKeyTTL(*ttl)
		if err != nil {
			return err
		}
		body := omitNil(obj{"name": *name, "scopes": splitList(*scopes), "team_id": *team})
		if expiresInS > 0 {
			body["expires_in_s"] = expiresInS
		}
		var out obj
		if err := a.c.call("POST", "/v1/keys", body, &out); err != nil {
			return err
		}
		return a.emit(out, func() {
			fmt.Fprintf(a.out, "%s\n(key %s; shown once, store it now)\n", s(out, "token"), s(out, "id"))
			if exp := s(out, "expires"); exp != "" {
				fmt.Fprintf(a.errw, "expires at %s\n", exp)
			}
		})
	default:
		if err := need(pos, 1, "KEY_ID"); err != nil {
			return err
		}
		if err := a.c.call("DELETE", "/v1/keys/"+url.PathEscape(pos[0]), nil, nil); err != nil {
			return err
		}
		return a.emit(obj{"ok": true, "id": pos[0]}, func() { fmt.Fprintln(a.out, "revoked", pos[0]) })
	}
}

func cmdTeams(a *app, args []string) error {
	op, args := sub(args, "ls", "ls", "create", "show", "rm", "add-member", "rm-member")
	fs := flag.NewFlagSet("teams", flag.ContinueOnError)
	role := fs.String("role", "member", "")
	owner := fs.String("owner", "", "create: new user who owns the team (operator only)")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	var out obj
	switch op {
	case "ls":
		l, err := listCall(a, "/v1/teams", "teams")
		if err != nil {
			return err
		}
		return a.emitList(l, "id", "name", "role")
	case "create":
		if err := need(pos, 1, "NAME"); err != nil {
			return err
		}
		body := obj{"name": pos[0]}
		if *owner != "" {
			body["owner"] = *owner
		}
		if err := a.c.call("POST", "/v1/teams", body, &out); err != nil {
			return err
		}
		return a.emit(out, func() {
			fmt.Fprintf(a.out, "created team %s (%s)\n", s(out, "name"), s(out, "id"))
			if o, ok := out["owner"].(map[string]any); ok {
				k, _ := o["key"].(map[string]any)
				fmt.Fprintf(a.out, "owner key: %v (shown once)\n", k["token"])
			}
		})
	case "rm":
		if err := need(pos, 1, "TEAM_ID"); err != nil {
			return err
		}
		if err := a.c.call("DELETE", "/v1/teams/"+url.PathEscape(pos[0]), nil, nil); err != nil {
			return err
		}
		return a.emit(obj{"ok": true, "id": pos[0]}, func() { fmt.Fprintln(a.out, "deleted team", pos[0]) })
	case "show":
		if err := need(pos, 1, "TEAM_ID"); err != nil {
			return err
		}
		if err := a.c.call("GET", "/v1/teams/"+url.PathEscape(pos[0]), nil, &out); err != nil {
			return err
		}
		return a.emit(out, func() {
			fmt.Fprintf(a.out, "%s %s (you: %s)\n", s(out, "id"), s(out, "name"), s(out, "role"))
			ms, _ := out["members"].([]any)
			for _, m := range ms {
				mm, _ := m.(map[string]any)
				fmt.Fprintf(a.out, "  %s\t%s\n", s(mm, "user_id"), s(mm, "role"))
			}
		})
	case "add-member":
		if err := need(pos, 2, "TEAM_ID NAME"); err != nil {
			return err
		}
		if err := a.c.call("POST", "/v1/teams/"+url.PathEscape(pos[0])+"/members", obj{"name": pos[1], "role": *role}, &out); err != nil {
			return err
		}
		return a.emit(out, func() {
			k, _ := out["key"].(map[string]any)
			u, _ := out["user"].(map[string]any)
			fmt.Fprintf(a.out, "added %v (%v) as %v\nkey: %v (shown once)\n", u["name"], u["id"], out["role"], k["token"])
		})
	default:
		if err := need(pos, 2, "TEAM_ID USER_ID"); err != nil {
			return err
		}
		if err := a.c.call("DELETE", "/v1/teams/"+url.PathEscape(pos[0])+"/members/"+url.PathEscape(pos[1]), nil, nil); err != nil {
			return err
		}
		return a.emit(obj{"ok": true, "team_id": pos[0], "user_id": pos[1]}, func() { fmt.Fprintln(a.out, "removed", pos[1]) })
	}
}

func cmdWebhooks(a *app, args []string) error {
	op, args := sub(args, "ls", "ls", "create", "rm", "deliveries", "test", "rotate", "show", "update")
	fs := flag.NewFlagSet("webhooks", flag.ContinueOnError)
	overlap := fs.Duration("overlap", -1, "how long both secrets sign (default 24h; 0 = at once)")
	newURL := fs.String("url", "", "")
	events := fs.String("events", "", "")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if op != "ls" {
		if err := need(pos, 1, "URL or WEBHOOK_ID"); err != nil {
			return err
		}
	}
	var out obj
	switch op {
	case "ls":
		l, err := listCall(a, "/v1/webhooks", "webhooks")
		if err != nil {
			return err
		}
		return a.emitList(l, "id", "url", "events")
	case "show":
		if err := a.c.call("GET", "/v1/webhooks/"+url.PathEscape(pos[0]), nil, &out); err != nil {
			return err
		}
		return a.show(out, "id", "url", "events", "prev_until")
	case "update":
		body := obj{}
		if *newURL != "" {
			body["url"] = *newURL
		}
		if *events != "" {
			body["events"] = splitList(*events)
		}
		if err := a.c.call("PATCH", "/v1/webhooks/"+url.PathEscape(pos[0]), body, &out); err != nil {
			return err
		}
		return a.show(out, "id", "url", "events")
	case "rotate":
		if err := a.c.call("POST", "/v1/webhooks/"+url.PathEscape(pos[0])+"/rotate", overlapBody(*overlap), &out); err != nil {
			return err
		}
		return a.emit(out, func() {
			fmt.Fprintf(a.out, "signing secret: %s (shown once)\n", s(out, "secret"))
			if u := s(out, "prev_until"); u != "" {
				fmt.Fprintf(a.errw, "deliveries carry both signatures until %s\n", u)
			}
		})
	case "create":
		if err := a.c.call("POST", "/v1/webhooks", omitNil(obj{"url": pos[0], "events": splitList(*events)}), &out); err != nil {
			return err
		}
		return a.emit(out, func() {
			fmt.Fprintf(a.out, "%s %s\nsigning secret: %s (shown once)\n", s(out, "id"), s(out, "url"), s(out, "secret"))
		})
	case "deliveries":
		l, err := listCall(a, "/v1/webhooks/"+url.PathEscape(pos[0])+"/deliveries", "deliveries")
		if err != nil {
			return err
		}
		return a.emitList(l, "at", "event", "attempt", "status", "ok", "error")
	case "test":
		if err := a.c.call("POST", "/v1/webhooks/"+url.PathEscape(pos[0])+"/test", nil, &out); err != nil {
			return err
		}
		return a.emit(out, func() { fmt.Fprintln(a.out, "queued", s(out, "event_id")) })
	default:
		if err := a.c.call("DELETE", "/v1/webhooks/"+url.PathEscape(pos[0]), nil, nil); err != nil {
			return err
		}
		return a.emit(obj{"ok": true, "id": pos[0]}, func() { fmt.Fprintln(a.out, "deleted", pos[0]) })
	}
}

func (a *app) secretExists(name string) bool {
	l, err := listCall(a, "/v1/secrets", "secrets")
	if err != nil {
		return true // let the PUT report the real error
	}
	for _, x := range l {
		if s(x, "name") == name {
			return true
		}
	}
	return false
}
