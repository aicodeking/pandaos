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
		"fork": cmdFork, "stop": vmAction("stop"), "start": vmAction("start"), "rm": cmdRm,
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

func cmdUsage(a *app, args []string) error {
	var u obj
	if err := a.c.call("GET", "/v1/usage", nil, &u); err != nil {
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
	image := fs.String("image", "", "")
	ttl := fs.Duration("ttl", 0, "")
	idle := fs.String("idle-stop", "", "auto-stop after this idle time (e.g. 30m), or never")
	snap := fs.String("snapshot", "", "create from a snapshot id")
	lab := labels{}
	fs.Var(lab, "label", "")
	secrets := fs.String("secrets", "", "comma-separated team secrets for this VM (default: all)")
	noSecrets := fs.Bool("no-secrets", false, "inject no team secrets")
	envName := fs.String("env", "", "environment name")
	fs.StringVar(envName, "environment", "", "same as --env")
	noEnv := fs.Bool("no-env", false, "ignore the team's default environment")
	from := fs.String("from", "", "a share code bsh_...")
	waitSetup := fs.Bool("wait-setup", false, "follow the environment's setup script; fail if it fails")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 1 && *name == "" {
		*name = pos[0]
	} else if len(pos) > 0 {
		return fmt.Errorf("unexpected arguments %v: %w", pos, errUsage)
	}
	body := obj{"name": *name, "size": *size, "image": *image, "ttl_s": int64(ttl.Seconds())}
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
		body["no_env"] = true
	}
	if *from != "" {
		body["from"] = *from
	}
	var vm obj
	if err := a.c.call("POST", "/v1/vms", body, &vm); err != nil {
		return err
	}
	if err := a.emitVM(vm); err != nil {
		return err
	}
	if se := s(vm, "setup_error"); se != "" {
		fmt.Fprintln(a.errw, "boat: setup:", se)
	}
	if c := s(vm, "setup_cmd_id"); c != "" && *waitSetup {
		return a.followLogs(vmPath(s(vm, "id"), "commands", c, "logs"))
	}
	return nil
}

func cmdList(a *app, args []string) error {
	l, err := listCall(a, "/v1/vms", "vms")
	if err != nil {
		return err
	}
	return a.emitList(l, vmCols...)
}

func oneVM(a *app, args []string) (obj, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("expected <vm>: %w", errUsage)
	}
	return a.c.resolveVM(args[0])
}

func vmAction(action string) func(*app, []string) error {
	return func(a *app, args []string) error {
		vm, err := oneVM(a, args)
		if err != nil {
			return err
		}
		var out obj
		if err := a.c.call("POST", vmPath(s(vm, "id"), action), nil, &out); err != nil {
			return err
		}
		return a.emitVM(out)
	}
}

func cmdRm(a *app, args []string) error {
	vm, err := oneVM(a, args)
	if err != nil {
		return err
	}
	if err := a.c.call("DELETE", vmPath(s(vm, "id")), nil, nil); err != nil {
		return err
	}
	return a.emit(obj{"ok": true, "id": s(vm, "id")}, func() { fmt.Fprintln(a.out, "deleted", s(vm, "id")) })
}

func cmdFork(a *app, args []string) error {
	fs := flag.NewFlagSet("fork", flag.ContinueOnError)
	name := fs.String("name", "", "")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	vm, err := oneVM(a, pos)
	if err != nil {
		return err
	}
	var out obj
	if err := a.c.call("POST", vmPath(s(vm, "id"), "fork"), obj{"name": *name}, &out); err != nil {
		return err
	}
	return a.emitVM(out)
}

// ---- exec / prompt ----

func cmdExec(a *app, args []string) error {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	timeout := fs.Int("timeout", 0, "seconds")
	cwd := fs.String("cwd", "", "")
	detach := fs.Bool("detach", false, "run in the background; prints the command id")
	pos, cmd, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 2 && len(cmd) == 0 { // boat exec vm "cmd string"
		cmd, pos = []string{"bash", "-lc", pos[1]}, pos[:1]
	}
	if len(cmd) == 0 {
		return fmt.Errorf("usage: boat exec <vm> -- cmd: %w", errUsage)
	}
	vm, err := oneVM(a, pos)
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
	model := fs.String("model", "", "")
	cwd := fs.String("cwd", "", "")
	timeout := fs.Int("timeout", 0, "seconds")
	var turn promptFlags
	turn.register(fs)
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
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
	if turn.used() {
		return a.startTurn(s(vm, "id"), obj{"prompt": text, "agent": *agent, "model": *model, "cwd": *cwd}, &turn)
	}
	return a.stream(vmPath(s(vm, "id"), "prompt"), obj{"prompt": text, "agent": *agent, "model": *model, "cwd": *cwd, "timeout_s": *timeout})
}

// stream relays exec JSON lines. --json passes them through verbatim;
// otherwise output is decoded to stdout/stderr. Returns the remote exit code.
func (a *app) stream(path string, body obj) error {
	b, _ := json.Marshal(body)
	resp, err := a.c.request("POST", path, bytes.NewReader(b), "application/json")
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
			Stream  string `json:"stream"`
			DataB64 string `json:"data_b64"`
			Exit    *int   `json:"exit"`
			Error   any    `json:"error"`
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
	pos, extra, err := parse(fs, args)
	if err != nil {
		return err
	}
	vm, err := oneVM(a, pos)
	if err != nil {
		return err
	}
	pub, err := a.publicKey(*ident)
	if err != nil {
		return err
	}
	var info obj
	if err := a.c.call("POST", vmPath(s(vm, "id"), "ssh"), obj{"public_key": pub}, &info); err != nil {
		return err
	}
	// VM ips are reused after a delete and every VM has its own host keys, so
	// known_hosts is keyed by VM id: a new VM on an old ip is not a "changed key".
	argv := []string{"ssh", "-p", s(info, "port"), "-o", "StrictHostKeyChecking=accept-new",
		"-o", "HostKeyAlias=boat-" + s(vm, "id")}
	if j := s(info, "jump"); j != "" {
		// accept-new here too: with no tty the inner ssh cannot ask about the jump host.
		argv = append(argv, "-o", "ProxyCommand=ssh -o StrictHostKeyChecking=accept-new -W %h:%p "+sshJumpArgs(j))
	}
	argv = append(argv, s(info, "user")+"@"+s(info, "host"))
	argv = append(argv, extra...)
	if a.json {
		info["argv"] = argv
		return a.emit(info, nil)
	}
	return a.execProg(argv)
}

// sshJumpArgs turns user@host:port into "-p port user@host".
func sshJumpArgs(j string) string {
	if i := strings.LastIndex(j, ":"); i > 0 && !strings.Contains(j[i:], "]") {
		return "-p " + j[i+1:] + " " + j[:i]
	}
	return j
}

// cmdSCP copies one file through the files API: boat scp a.txt vm:/tmp/a.txt
func cmdSCP(a *app, args []string) error {
	if err := need(args, 2, "<src> <dst> with vm:/path on one side"); err != nil {
		return err
	}
	src, dst := args[0], args[1]
	srcVM, srcPath, srcRemote := splitRemote(src)
	dstVM, dstPath, dstRemote := splitRemote(dst)
	switch {
	case srcRemote == dstRemote:
		return fmt.Errorf("exactly one of src/dst must be vm:/path: %w", errUsage)
	case dstRemote:
		vm, err := a.c.resolveVM(dstVM)
		if err != nil {
			return err
		}
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		if strings.HasSuffix(dstPath, "/") {
			dstPath += filepath.Base(src)
		}
		resp, err := a.c.request("PUT", vmPath(s(vm, "id"), "files")+"?path="+url.QueryEscape(dstPath), f, "application/octet-stream")
		if err != nil {
			return err
		}
		var out obj
		json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		return a.emit(out, func() { fmt.Fprintf(a.out, "%s -> %s:%s (%v bytes)\n", src, dstVM, dstPath, out["bytes"]) })
	default:
		vm, err := a.c.resolveVM(srcVM)
		if err != nil {
			return err
		}
		resp, err := a.c.request("GET", vmPath(s(vm, "id"), "files")+"?path="+url.QueryEscape(srcPath), nil, "")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if st, err := os.Stat(dst); err == nil && st.IsDir() {
			dst = filepath.Join(dst, filepath.Base(srcPath))
		}
		f, err := os.Create(dst)
		if err != nil {
			return err
		}
		n, err := io.Copy(f, resp.Body)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		return a.emit(obj{"ok": true, "path": dst, "bytes": n}, func() { fmt.Fprintf(a.out, "%s -> %s (%d bytes)\n", src, dst, n) })
	}
}

func splitRemote(p string) (vm, path string, remote bool) {
	vm, path, ok := strings.Cut(p, ":")
	if !ok || vm == "" || strings.ContainsAny(vm, `/\`) || !strings.HasPrefix(path, "/") {
		return "", p, false
	}
	return vm, path, true
}

func cmdDesktop(a *app, args []string) error {
	fs := flag.NewFlagSet("desktop", flag.ContinueOnError)
	noOpen := fs.Bool("no-open", false, "")
	pos, _, err := parse(fs, args)
	if err != nil {
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
	if a.json || *noOpen {
		return a.emit(d, func() { fmt.Fprintln(a.out, s(d, "url")) })
	}
	fmt.Fprintln(a.out, s(d, "url"))
	return a.open(s(d, "url"))
}

// ---- ports / snapshots ----

func cmdPorts(a *app, args []string) error {
	op, args := sub(args, "ls", "ls", "add", "rm", "url")
	fs := flag.NewFlagSet("ports", flag.ContinueOnError)
	public := fs.Bool("public", false, "open route + raw TCP port (same as --auth public)")
	auth := fs.String("auth", "", "private (default: gateway route needs a token from `boat ports url`) or public")
	ttl := fs.Duration("ttl", 0, "lifetime of a private port URL (default: server's, 1h)")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	want := 2
	if op == "ls" {
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
	default:
		if err := a.c.call("DELETE", vmPath(id, "ports", url.PathEscape(pos[1])), nil, nil); err != nil {
			return err
		}
		return a.emit(obj{"ok": true, "port": pos[1]}, func() { fmt.Fprintln(a.out, "removed port", pos[1]) })
	}
}

func cmdSnapshots(a *app, args []string) error {
	if len(args) == 0 {
		return cmdSnapshotsTeam(a, "team", nil)
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
		return a.emitList(l, "id", "name", "created", "size_bytes")
	case "create":
		var out obj
		if err := a.c.call("POST", vmPath(id, "snapshots"), obj{"name": *name}, &out); err != nil {
			return err
		}
		return a.emitList([]obj{out}, "id", "name", "created")
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

// ---- secrets / keys / teams / webhooks ----

func cmdSecrets(a *app, args []string) error {
	op, pos := sub(args, "ls", "ls", "set", "rotate", "rm")
	switch op {
	case "ls":
		l, err := listCall(a, "/v1/secrets", "secrets")
		if err != nil {
			return err
		}
		return a.emitList(l, "name", "updated")
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
		var out obj
		if err := a.c.call("PUT", "/v1/secrets/"+url.PathEscape(pos[0]), obj{"value": value}, &out); err != nil {
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

func cmdKeys(a *app, args []string) error {
	op, args := sub(args, "ls", "ls", "create", "rm", "rotate", "usage")
	fs := flag.NewFlagSet("keys", flag.ContinueOnError)
	overlap := fs.Duration("overlap", -1, "how long the old secret keeps working (default 24h; 0 = revoke now)")
	name := fs.String("name", "", "")
	scopes := fs.String("scopes", "", "")
	team := fs.String("team", "", "")
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
		var out obj
		if err := a.c.call("POST", "/v1/keys", omitNil(obj{"name": *name, "scopes": splitList(*scopes), "team_id": *team}), &out); err != nil {
			return err
		}
		return a.emit(out, func() {
			fmt.Fprintf(a.out, "%s\n(key %s; shown once, store it now)\n", s(out, "token"), s(out, "id"))
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
