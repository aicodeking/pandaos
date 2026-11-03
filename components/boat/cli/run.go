package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
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
	"sync"
	"syscall"
	"time"
)

func init() {
	commands["run"] = cmdRun
	commands["push"] = cmdPush
}

const (
	// runIdleStopS stops a job's VM if the CLI dies: a running exec counts as
	// activity, so a live job is never stopped.
	runIdleStopS = 15 * 60
	// runNoLimitS stands for "no limit": the agent's default exec timeout is 600 s.
	runNoLimitS = 30 * 24 * 60 * 60
	maxBatch    = 1000
	batchJobs   = 8
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

// newBatch is `boat new --count N`: N creates, all or none. A share code is
// imported once, so the batch shares one snapshot.
//
// ponytail: client-side loop with rollback, not atomic if the CLI dies midway;
// replace with a server batch when POST /v1/vms takes count.
func (a *app) newBatch(body obj, n int) error {
	if code, ok := body["from"]; ok {
		var snap obj
		if err := a.c.call("POST", "/v1/snapshots/import-share", obj{"code": code}, &snap); err != nil {
			return err
		}
		delete(body, "from")
		body["snapshot"] = s(snap, "id")
	}
	vms, errs := make([]obj, n), make([]error, n)
	sem := make(chan struct{}, batchJobs)
	var wg sync.WaitGroup
	for i := range vms {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			errs[i] = a.c.call("POST", "/v1/vms", body, &vms[i])
		}(i)
	}
	wg.Wait()
	first := errors.Join(errs...)
	if first == nil {
		return a.emitList(vms, vmCols...)
	}
	for i, vm := range vms {
		if errs[i] != nil {
			continue
		}
		if err := a.c.call("DELETE", vmPath(s(vm, "id")), nil, nil); err != nil {
			fmt.Fprintf(a.errw, "boat: rollback could not delete %s: %v\n", s(vm, "id"), err)
		}
	}
	return fmt.Errorf("the batch did not fit, so it was rolled back: %w", first)
}

// quiet runs a command and returns its stdout; output never reaches the user.
func (a *app) quiet(id string, argv []string) (string, error) {
	var out, errb bytes.Buffer
	q := *a
	q.out, q.errw, q.json = &out, &errb, false
	if err := q.stream(vmPath(id, "exec"), obj{"cmd": argv}); err != nil {
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
	resp, err := a.c.request("PUT", vmPath(id, "files")+"?path="+url.QueryEscape(tmp), pr, "application/octet-stream")
	if err != nil {
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
	fl.Var(envv, "e", "KEY=VALUE (repeatable)")
	fl.Var(envv, "env", "same as -e")
	environment := fl.String("environment", "", "environment name")
	noEnv := fl.Bool("no-env", false, "the VM gets none of the team's stored secrets")
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
	body := obj{"size": *size, "idle_stop_s": runIdleStopS}
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
		body["secrets"] = []string{}
	}
	// Before the create: Ctrl-C during it still deletes the VM it made.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	var vm obj
	if err := a.c.call("POST", "/v1/vms", body, &vm); err != nil {
		return err
	}
	return a.runJob(vm, o, sig)
}

func (a *app) runJob(vm obj, o runOpts, sig <-chan os.Signal) error {
	id := s(vm, "id")
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- a.runSteps(vm, o) }()
	var err error
	select {
	case err = <-done:
	case <-sig:
		err = exitError{130}
	}
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

// runSteps waits for setup, pushes, and runs the command.
func (a *app) runSteps(vm obj, o runOpts) error {
	id := s(vm, "id")
	if c := s(vm, "setup_cmd_id"); c != "" {
		q := *a
		q.out, q.json = a.errw, false
		if err := q.followLogs(vmPath(id, "commands", c, "logs")); err != nil {
			return fmt.Errorf("environment setup failed: %w", err)
		}
	}
	cwd := o.cwd
	if o.pushDir != "" {
		r, err := a.push(id, o.pushDir, "")
		if err != nil {
			return err
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
	body["timeout_s"] = o.timeoutS
	if o.timeoutS == 0 {
		body["timeout_s"] = runNoLimitS
	}
	return a.stream(vmPath(id, "exec"), body)
}
