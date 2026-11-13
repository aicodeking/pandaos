package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Round 12: boat.dev's CLI reference (docs.boat.dev/cli-reference.md), verb by
// verb. Flags this server cannot honor are accepted by the parser and then
// fail with why (refused), so a script gets a reason, not "flag not defined".

// secsFlag is boat.dev's --ttl SECONDS; a Go duration (2h) also works.
type secsFlag struct {
	v   int64
	set bool
}

func (f *secsFlag) String() string { return strconv.FormatInt(f.v, 10) }
func (f *secsFlag) Set(s string) error {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		d, derr := time.ParseDuration(s)
		if derr != nil {
			return errors.New("want seconds, or a duration like 2h")
		}
		n = int64(d.Seconds())
	}
	if n < 0 || n > maxTTLSeconds {
		return fmt.Errorf("want 0-%d seconds", maxTTLSeconds)
	}
	f.v, f.set = n, true
	return nil
}

// envFlag is boat.dev's --env: KEY=VALUE is a variable for this VM; a bare
// name selects an environment (this CLI's older meaning, same as --environment).
type envFlag struct {
	name *string
	vars labels
}

func (f envFlag) String() string { return "" }
func (f envFlag) Set(s string) error {
	if strings.Contains(s, "=") {
		return f.vars.Set(s)
	}
	*f.name = s
	return nil
}

// refused is a boat.dev flag with no equivalent here: parsing it records why.
type refused struct {
	why    string
	isBool bool
	hit    *string
}

func (r refused) String() string   { return "" }
func (r refused) IsBoolFlag() bool { return r.isBool }
func (r refused) Set(string) error {
	if *r.hit == "" {
		*r.hit = r.why
	}
	return nil
}

// refuse declares boat.dev flags fs cannot honor; after parse, *hit says why.
// Names ending in "=" take a value.
func refuse(fs *flag.FlagSet, hit *string, why string, names ...string) {
	for _, n := range names {
		name, takesValue := strings.CutSuffix(n, "=")
		fs.Var(refused{why: "--" + name + " is not supported on self-hosted boat: " + why, isBool: !takesValue, hit: hit}, name, "not supported here")
	}
}

func refusedErr(hit string) error {
	if hit != "" {
		return errors.New(hit)
	}
	return nil
}

// newExtras are boat new's follow-ups after the VM exists.
type newExtras struct {
	setupFile string
	files     multi // SANDBOX_PATH=LOCAL_FILE
	hosts     multi // PORT or PORT:public
}

func (x *newExtras) register(fs *flag.FlagSet) {
	fs.StringVar(&x.setupFile, "setup-file", "", "run this local script on the VM once it is up (in the background)")
	fs.Var(&x.files, "file", "SANDBOX_PATH=LOCAL_FILE: write a local file on the VM (repeatable)")
	fs.Var(&x.hosts, "host", "PORT or PORT:public: host a port as boat host does (repeatable)")
}

func (x *newExtras) used() bool { return x.setupFile != "" || len(x.files) > 0 || len(x.hosts) > 0 }

// check reads the local files before anything is created.
func (x *newExtras) check() error {
	for _, f := range x.files {
		dst, src, ok := strings.Cut(f, "=")
		if !ok || dst == "" || src == "" {
			return fmt.Errorf("--file wants SANDBOX_PATH=LOCAL_FILE: %w", errUsage)
		}
		if _, err := os.Stat(src); err != nil {
			return err
		}
	}
	for _, h := range x.hosts {
		port, mode, _ := strings.Cut(h, ":")
		if _, err := portArg("host", port); err != nil {
			return err
		}
		if mode != "" && mode != "public" && mode != "private" {
			return fmt.Errorf("--host wants PORT or PORT:public: %w", errUsage)
		}
	}
	if x.setupFile != "" {
		if st, err := os.Stat(x.setupFile); err != nil {
			return err
		} else if st.Size() > 64<<10 {
			return fmt.Errorf("--setup-file is over 64KB: %w", errUsage)
		}
	}
	return nil
}

// apply writes the files, starts the setup script and hosts the ports, in
// boat.dev's order (files before setup); errors go to stderr, as boat.dev's
// setupError does, and the VM stays.
func (x *newExtras) apply(a *app, id string) []obj {
	q := *a
	q.json = true // the follow-ups print nothing of their own
	var discard strings.Builder
	q.out = &discard
	for _, f := range x.files {
		dst, src, _ := strings.Cut(f, "=")
		if err := q.scpFileUp(id, src, id, dst); err != nil {
			fmt.Fprintf(a.errw, "boat: setup: --file %s: %v\n", dst, err)
		}
	}
	if x.setupFile != "" {
		b, err := os.ReadFile(x.setupFile)
		if err == nil {
			err = a.c.call("POST", vmPath(id, "commands"), obj{"cmd": []string{"bash", "-lc", string(b)}}, nil)
		}
		if err != nil {
			fmt.Fprintf(a.errw, "boat: setup: --setup-file: %v\n", err)
		}
	}
	var hosted []obj
	for _, h := range x.hosts {
		port, mode, _ := strings.Cut(h, ":")
		u, err := a.hostPort(id, port, mode == "public")
		if err != nil {
			fmt.Fprintf(a.errw, "boat: --host %s: %v\n", h, err)
			continue
		}
		hosted = append(hosted, u)
		if !a.json {
			fmt.Fprintf(a.out, "%s\t%s\n", port, s(u, "url"))
		}
	}
	return hosted
}

// hostPort exposes port and returns boat.dev's hosted-port entry.
func (a *app) hostPort(id, portStr string, public bool) (obj, error) {
	port, err := portArg("port", portStr)
	if err != nil {
		return nil, err
	}
	access := "private"
	if public {
		access = "public"
	}
	if err := a.c.call("POST", vmPath(id, "ports"), obj{"port": port, "auth": access}, nil); err != nil {
		return nil, err
	}
	var u obj
	p := vmPath(id, "ports", url.PathEscape(portStr), "url") + "?ttl_s=" + strconv.Itoa(hostURLTTLS)
	if err := a.c.call("GET", p, nil, &u); err != nil {
		return nil, err
	}
	return obj{"sandboxId": id, "port": port, "url": s(u, "url"), "access": access,
		"isProtected": access == "private", "localhost": false, "expires_at": u["expires_at"]}, nil
}

// setTTL is boat.dev's --ttl / --no-auto-stop after a fork or resume (the
// native calls keep the VM's own lifetime): PATCH ttl_s, 0 = no auto-stop.
func (a *app) setTTL(id string, ttl secsFlag, never bool) error {
	switch {
	case never:
		return a.c.call("PATCH", vmPath(id), obj{"ttl_s": 0}, nil)
	case ttl.set:
		return a.c.call("PATCH", vmPath(id), obj{"ttl_s": ttl.v}, nil)
	}
	return nil
}

func ttlConflict(ttl secsFlag, never bool) error {
	if ttl.set && never {
		return fmt.Errorf("--ttl and --no-auto-stop conflict: %w", errUsage)
	}
	return nil
}

// cmdStopStatus: boat stop <vm> --status STOP_ID reads a stop operation.
func cmdStopStatus(a *app, id, stopID string) error {
	var out struct {
		Operation obj `json:"operation"`
	}
	if err := a.c.call("GET", compatVMPath(id, "stops", url.PathEscape(stopID)), nil, &out); err != nil {
		return err
	}
	return a.show(out.Operation, "id", "status", "sandboxId", "createdAt")
}

// cmdHealth is boat.dev's boat status with no VM: the API, the key, the
// config and this binary. --check exits 1 when not logged in.
func cmdHealth(a *app, check bool) error {
	cfg, _ := loadConfig(a.getenv)
	exe, _ := os.Executable()
	v := obj{"api": obj{"url": cfg.URL}, "account": obj{"signedIn": false}, "config": configPath(a.getenv), "binary": exe, "version": version}
	if cfg.URL != "" && cfg.Key != "" {
		c := &client{base: cfg.URL, key: cfg.Key, hc: &http.Client{Timeout: 10 * time.Second}}
		var me obj
		if err := c.call("GET", "/v1/me", nil, &me); err == nil {
			v["account"] = obj{"signedIn": true, "user": me["user"], "team": me["team"]}
			v["api"] = obj{"url": cfg.URL, "ok": true}
		} else {
			v["api"] = obj{"url": cfg.URL, "ok": false, "error": err.Error()}
		}
	}
	signedIn := v["account"].(obj)["signedIn"] == true
	err := a.emit(v, func() {
		api := v["api"].(obj)
		fmt.Fprintf(a.out, "api:      %s (reachable: %v)\nsigned in: %v\nconfig:   %s\nbinary:   %s\nversion:  %s\n",
			orStr(cfg.URL, "(none)"), api["ok"] == true, signedIn, v["config"], exe, version)
	})
	if err == nil && check && !signedIn {
		return exitError{1}
	}
	return err
}

// convUUID is the API's conversation id form for a native conv_ id.
func convUUID(conv string) string {
	h, ok := strings.CutPrefix(conv, "conv_")
	if !ok || len(h) != 16 {
		return conv
	}
	return h[:8] + "-" + h[8:12] + "-" + h[12:] + "-8000-000000000000"
}

// convNative turns the API's conversation UUID back into a native conv_ id.
func convNative(c string) string {
	if len(c) == 36 && strings.HasSuffix(c, "-8000-000000000000") {
		return "conv_" + strings.ReplaceAll(c[:18], "-", "")
	}
	return c
}

// conversation resolves --convo: an id, or current (the VM's newest prompt's).
func (a *app) conversation(id, convo string) (string, error) {
	if convo != "current" {
		return convNative(convo), nil
	}
	l, err := listCall(a, vmPath(id, "prompts"), "prompts")
	if err != nil {
		return "", err
	}
	for i := len(l) - 1; i >= 0; i-- {
		if c := s(l[i], "conversation_id"); c != "" {
			return c, nil
		}
	}
	return "", errors.New("no conversation on this VM yet")
}

// runningTurns: the running prompts of id, of conversation conv ("" = all).
func (a *app) runningTurns(id, conv string) ([]string, error) {
	l, err := listCall(a, vmPath(id, "prompts"), "prompts")
	if err != nil {
		return nil, err
	}
	var out []string
	for i := len(l) - 1; i >= 0; i-- {
		if s(l[i], "state") == "running" && (conv == "" || s(l[i], "conversation_id") == conv) {
			out = append(out, s(l[i], "id"))
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no_running_turn: no turn is running on this VM; use boat prompt")
	}
	return out, nil
}

// attach uploads prompt attachments to ~/attachments and names them in the prompt.
func (a *app) attach(id string, files []string, text string) (string, error) {
	if len(files) == 0 {
		return text, nil
	}
	if _, err := a.quiet(id, []string{"mkdir", "-p", "attachments"}); err != nil {
		return "", err
	}
	q := *a
	q.json = true
	var discard strings.Builder
	q.out = &discard
	var names []string
	for _, f := range files {
		dst := "attachments/" + filepath.Base(f)
		if err := q.scpFileUp(id, f, id, dst); err != nil {
			return "", fmt.Errorf("--attach %s: %w", f, err)
		}
		names = append(names, "~/"+dst)
	}
	return text + "\n\nAttached files: " + strings.Join(names, ", "), nil
}

func firstOr(l []string, def string) string {
	if len(l) > 0 {
		return l[0]
	}
	return def
}
