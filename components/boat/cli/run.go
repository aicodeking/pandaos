package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// runHeartbeatEvery is how often a live `boat run` renews the VM's auto-stop
// deadline; a variable so tests can shorten it.
var runHeartbeatEvery = 5 * time.Minute

func init() {
	commands["run"] = cmdRun
	commands["push"] = cmdPush
}

const (
	// runIdleStopS stops a job's VM if the CLI dies: a running exec counts as
	// activity, so a live job is never stopped.
	runIdleStopS = 15 * 60
	// runTTLS is boat.dev's 15-minute wall-clock auto-stop for a job's VM. The
	// live CLI pushes it forward every runHeartbeatEvery (PATCH ttl_s sets the
	// time left from now), so a guest command that outlives a dead CLI cannot
	// keep the VM up past it.
	runTTLS = 15 * 60
	// runNoLimitS stands for "no limit": the agent's default exec timeout is 600 s.
	runNoLimitS = 30 * 24 * 60 * 60
	maxBatch    = 16 // boatd's all-or-none batch (POST /v1/vms count)
)

// applyFrom sets the create body from --from: a share code or a snapshot id/name.
func (a *app) applyFrom(body obj, from string) error {
	if from == "" {
		return nil
	}
	if strings.HasPrefix(from, "bsh_") {
		body["from"] = from
		return nil
	}
	l, err := listCall(a, "/v1/snapshots", "snapshots")
	if err != nil {
		return err
	}
	var hits []string
	for _, x := range l {
		if s(x, "id") == from || s(x, "name") == from {
			hits = append(hits, s(x, "id"))
		}
	}
	switch len(hits) {
	case 0:
		return fmt.Errorf("no snapshot with id or name %q", from)
	case 1:
		body["snapshot"] = hits[0]
		return nil
	}
	return fmt.Errorf("snapshot name %q is ambiguous (%d snapshots); use the id", from, len(hits))
}

// newBatch is `boat new --count N`: one request, all or none on the server
// (boatd creates the batch under one lock and deletes it all if one fails).
func (a *app) newBatch(body obj, n int) error {
	body["count"] = n
	var out struct {
		VMs []obj `json:"vms"`
	}
	if err := a.c.call("POST", "/v1/vms", body, &out); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Code == "env_start_failed" {
			return a.createStartFailure(err)
		}
		return fmt.Errorf("the batch was not created (all or none): %w", err)
	}
	failed := 0
	for _, vm := range out.VMs {
		ee := strings.TrimSpace(s(vm, "env_error"))
		if ee != "" {
			failed++
		}
		if a.json {
			if ee != "" {
				a.failEnv(vm, ee) // one error event with the id
			} else {
				a.emit(readyEvent(vm), nil)
			}
		}
	}
	if !a.json {
		if err := a.emitList(markEnvFailed(out.VMs, false), vmCols...); err != nil {
			return err
		}
		for _, vm := range out.VMs {
			if ee := strings.TrimSpace(s(vm, "env_error")); ee != "" {
				a.failEnv(vm, ee)
			}
		}
	}
	if failed > 0 {
		return exitError{1}
	}
	return nil
}

// quiet runs a command and returns its stdout; output never reaches the user.
func (a *app) quiet(id string, argv []string) (string, error) {
	var out, errb bytes.Buffer
	q := *a
	q.out, q.errw, q.json = &out, &errb, false
	body := obj{"cmd": argv}
	if dl, ok := a.context().Deadline(); ok {
		rem := time.Until(dl)
		if rem <= 0 {
			return "", context.DeadlineExceeded
		}
		body["timeout_s"] = max(1, int(rem.Seconds()))
	}
	if err := q.stream(vmPath(id, "exec"), body); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// ---- push ----

// listPushFiles returns the relative paths to send. In a git repository: what
// git tracks plus new files .gitignore does not exclude, with submodules.
// Elsewhere: everything but .git.
func listPushFiles(dir string) ([]string, error) {
	out, err := exec.Command("git", "-C", dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		return walkPushFiles(dir)
	}
	names := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	slices.Sort(names)
	var files []string
	for _, n := range slices.Compact(names) {
		if n == "" {
			continue
		}
		fi, err := os.Lstat(filepath.Join(dir, n))
		switch {
		case errors.Is(err, fs.ErrNotExist): // tracked, deleted here
		case err != nil:
			return nil, err
		case fi.IsDir(): // a submodule
			sub, err := listPushFiles(filepath.Join(dir, n))
			if err != nil {
				return nil, err
			}
			for _, x := range sub {
				files = append(files, filepath.Join(n, x))
			}
		default:
			files = append(files, filepath.FromSlash(n))
		}
	}
	return files, nil
}

func walkPushFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.Name() == ".git" && d.IsDir():
			return fs.SkipDir
		case d.Name() == ".git" || d.IsDir():
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err == nil {
			files = append(files, rel)
		}
		return err
	})
	return files, err
}

// packPush writes a .tgz that keeps the exec bit and keeps links as links.
func packPush(w io.Writer, dir string, files []string) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, name := range files {
		if err := packOne(tw, filepath.Join(dir, name), name); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func packOne(tw *tar.Writer, path, name string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	link := ""
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		if link, err = os.Readlink(path); err != nil {
			return err
		}
	case !fi.Mode().IsRegular():
		return nil // sockets, fifos
	}
	h, err := tar.FileInfoHeader(fi, link)
	if err != nil {
		return err
	}
	h.Name = filepath.ToSlash(name)
	h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", "" // the VM user owns what it unpacks
	if err := tw.WriteHeader(h); err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(tw, f)
	return err
}

// The archive lands as root in sticky /tmp; a path outside home may need root.
const unpackScript = `d=$0; case "$d" in /*) ;; *) d=$HOME/$d ;; esac
mkdir -p -- "$d" 2>/dev/null || sudo -n install -d -o "$(id -u)" -g "$(id -g)" -- "$d" || exit 1
tar xzf "$1" -C "$d"; rc=$?
rm -f -- "$1" 2>/dev/null || sudo -n rm -f -- "$1"
[ "$rc" = 0 ] && printf '%s\n' "$d"; exit "$rc"`

type pushResult struct {
	Path  string
	Files int
}

// push sends dir to the VM and unpacks it at `to` (relative = from home).
//
// ponytail: goes through the files API, so one archive is capped by the API's
// -max-file-bytes (256 MiB by default); boat.dev streams over SSH with no cap.
func (a *app) push(id, dir, to string) (pushResult, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return pushResult{}, err
	}
	files, err := listPushFiles(abs)
	if err != nil {
		return pushResult{}, err
	}
	return a.pushFiles(id, abs, files, to)
}

// pushFiles sends files (relative to abs) and unpacks them at `to`
// (default ~/<base of abs>; relative = from home).
func (a *app) pushFiles(id, abs string, files []string, to string) (pushResult, error) {
	if to == "" {
		to = filepath.Base(abs)
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return pushResult{}, err
	}
	tmp := "/tmp/.boat-push-" + hex.EncodeToString(rnd[:]) + ".tgz"
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(packPush(pw, abs, files)) }()
	resp, err := a.c.requestCtx(a.context(), "PUT", vmPath(id, "files")+"?path="+url.QueryEscape(tmp), pr, "application/octet-stream")
	if err != nil {
		pr.CloseWithError(err)
		return pushResult{}, err
	}
	resp.Body.Close()
	out, err := a.quiet(id, []string{"bash", "-c", unpackScript, to, tmp})
	if err != nil {
		return pushResult{}, fmt.Errorf("unpack on the VM: %w", err)
	}
	return pushResult{Path: strings.TrimSpace(out), Files: len(files)}, nil
}

func checkPushDir(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	return nil
}

func cmdPush(a *app, args []string) error {
	fl := flag.NewFlagSet("push", flag.ContinueOnError)
	to := fl.String("to", "", "directory on the VM (default: ~/<dir name>)")
	pos, _, err := parse(fl, args)
	if err != nil {
		return err
	}
	if err := need(pos, 2, "<vm> <dir>"); err != nil {
		return err
	}
	if err := checkPushDir(pos[1]); err != nil {
		return err
	}
	id, err := vmArg(a, pos[0])
	if err != nil {
		return err
	}
	r, err := a.push(id, pos[1], *to)
	if err != nil {
		return err
	}
	return a.okLine(obj{"ok": true, "id": id, "path": r.Path, "files": r.Files},
		fmt.Sprintf("pushed %d files to %s:%s", r.Files, id, r.Path))
}

// ---- run ----

type runOpts struct {
	cmd      []string
	pushDir  string
	cwd      string
	timeoutS int
	keep     bool
}

func cmdRun(a *app, args []string) error {
	fl := flag.NewFlagSet("run", flag.ContinueOnError)
	from := fl.String("from", "", "a saved snapshot (id or name) or a share code")
	size := fl.String("type", "", "small, default, large or xlarge")
	fl.StringVar(size, "size", "", "same as --type")
	var o runOpts
	fl.StringVar(&o.pushDir, "push", "", "copy this directory in first and run there")
	fl.StringVar(&o.cwd, "cwd", "", "working directory, relative to home")
	fl.IntVar(&o.timeoutS, "timeout", 0, "stop the command after this many seconds and exit 124")
	fl.BoolVar(&o.keep, "keep", false, "keep the VM afterwards (it stops 15 minutes later)")
	envv := labels{}
	fl.Var(envVarFlag{envv}, "e", "KEY=VALUE (repeatable)")
	fl.Var(envVarFlag{envv}, "env", "same as -e")
	environment := fl.String("environment", "", "environment name")
	noEnv := fl.Bool("no-env", false, "ignore the team's default environment and stored secrets")
	pos, after, err := parse(fl, args)
	if err != nil {
		return err
	}
	o.cmd = append(pos, after...)
	switch {
	case len(o.cmd) == 0:
		return fmt.Errorf("usage: boat run [flags] -- cmd [args]: %w", errUsage)
	case o.timeoutS < 0:
		return fmt.Errorf("--timeout must be >= 0: %w", errUsage)
	}
	if o.pushDir != "" {
		if err := checkPushDir(o.pushDir); err != nil {
			return err
		}
	}
	body := obj{"size": *size, "idle_stop_s": runIdleStopS, "ttl_s": runTTLS}
	if err := a.applyFrom(body, *from); err != nil {
		return err
	}
	if len(envv) > 0 {
		body["env"] = envv
	}
	if *environment != "" {
		body["environment"] = *environment
	}
	if *noEnv {
		body["no_env"] = true
		body["secrets"] = []string{}
	}
	// Before the create: Ctrl-C during it still deletes the VM it made.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	var vm obj
	if err := a.c.call("POST", "/v1/vms", body, &vm); err != nil {
		return a.createStartFailure(err)
	}
	return a.runJob(vm, o, sig)
}

func (a *app) runJob(vm obj, o runOpts, sig <-chan os.Signal) error {
	id := s(vm, "id")
	start := time.Now()
	ctx := a.context()
	var deadline time.Time
	var cancel context.CancelFunc = func() {}
	if o.timeoutS > 0 {
		deadline = start.Add(time.Duration(o.timeoutS) * time.Second)
		ctx, cancel = context.WithDeadline(ctx, deadline)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- a.runSteps(ctx, vm, o, deadline) }()
	hbCtx, stopBeat := context.WithCancel(context.Background())
	beatDone := make(chan struct{})
	go func() {
		defer close(beatDone)
		a.heartbeat(hbCtx, id)
	}()
	var err error
	select {
	case err = <-done:
	case <-sig:
		cancel()
		err = exitError{130}
	}
	// The job is over: stop renewing, and wait so no PATCH lands after the DELETE.
	stopBeat()
	<-beatDone
	code := 0
	var ee exitError
	switch {
	case errors.As(err, &ee):
		code = ee.code
	case err != nil:
		code = 1
	}
	deleted := false
	if !o.keep {
		if derr := a.c.call("DELETE", vmPath(id), nil, nil); derr != nil {
			fmt.Fprintf(a.errw, "boat: could not delete %s: %v\n", id, derr)
		} else {
			deleted = true
		}
	}
	if o.keep {
		a.setCurrent(id)
	}
	elapsed := time.Since(start).Round(time.Second)
	if a.json {
		if jerr := a.emit(obj{"type": "summary", "exitCode": code, "elapsedSeconds": int64(elapsed.Seconds()),
			"sandboxType": s(vm, "size"), "sandboxId": id, "deleted": deleted}, nil); jerr != nil {
			return jerr
		}
	} else {
		state := "kept"
		if deleted {
			state = "deleted"
		} else if !o.keep {
			state = "not deleted"
		}
		fmt.Fprintf(a.errw, "exit %d · %s · %s · %s %s\n", code, elapsed, s(vm, "size"), id, state)
	}
	return err
}

// heartbeat renews the VM's wall-clock auto-stop until ctx ends. Best effort:
// a failed renewal is not fatal (the next tick retries, and the VM may be gone).
func (a *app) heartbeat(ctx context.Context, id string) {
	tick := time.NewTicker(runHeartbeatEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			a.renewTTL(ctx, id)
		}
	}
}

// renewTTL is one PATCH ttl_s; ctx aborts it so a hung call cannot hold up the job's exit.
func (a *app) renewTTL(ctx context.Context, id string) {
	b, _ := json.Marshal(obj{"ttl_s": runTTLS})
	resp, err := a.c.requestCtx(ctx, "PATCH", vmPath(id), bytes.NewReader(b), "application/json")
	if err == nil {
		resp.Body.Close()
	}
}

func (a *app) waitSetupCmd(ctx context.Context, id, cmdID string) error {
	q := *a
	q.ctx, q.out, q.json = ctx, a.errw, false
	if err := q.followLogs(vmPath(id, "commands", cmdID, "logs")); err != nil {
		return fmt.Errorf("environment setup failed: %w", err)
	}
	return nil
}

// runSteps waits for setup, pushes, and runs the command.
func (a *app) runSteps(ctx context.Context, vm obj, o runOpts, deadline time.Time) error {
	id := s(vm, "id")
	q := *a
	q.ctx = ctx
	timed := !deadline.IsZero()
	mapDeadline := func(err error) error {
		if timed && errors.Is(err, context.DeadlineExceeded) {
			return exitError{124}
		}
		return err
	}
	if ee := strings.TrimSpace(s(vm, "env_error")); ee != "" {
		return fmt.Errorf("environment delivery failed: %s", ee)
	}
	if se := strings.TrimSpace(s(vm, "setup_error")); se != "" {
		return fmt.Errorf("environment setup failed: %s", se)
	}
	if c := strings.TrimSpace(s(vm, "setup_cmd_id")); c != "" {
		if err := a.waitSetupCmd(ctx, id, c); err != nil {
			return mapDeadline(err)
		}
	}
	cwd := o.cwd
	if o.pushDir != "" {
		r, err := q.push(id, o.pushDir, "")
		if err != nil {
			return mapDeadline(err)
		}
		if cwd == "" {
			cwd = r.Path
		}
	}
	cmd := o.cmd
	body := obj{}
	switch {
	case strings.HasPrefix(cwd, "/"):
		body["cwd"] = cwd
	case cwd != "": // relative to home, which only the VM knows
		cmd = append([]string{"bash", "-c", `cd -- "$HOME/$0" && exec "$@"`, cwd}, cmd...)
	}
	body["cmd"] = cmd
	if timed {
		rem := time.Until(deadline)
		if rem <= 0 {
			return exitError{124}
		}
		body["timeout_s"] = max(1, int(rem.Seconds()))
	} else {
		body["timeout_s"] = runNoLimitS
	}
	return mapDeadline(q.stream(vmPath(id, "exec"), body))
}
