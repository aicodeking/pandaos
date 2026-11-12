package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// boat.dev verbs this server backs through /api/v1 (which takes native ids as
// well as bx_ ids), and the ones it cannot do at all.

func init() {
	for k, v := range map[string]func(*app, []string) error{
		"status": cmdStatus, "events": cmdEvents, "recover": cmdRecover,
		"share": cmdShare, "unshare": cmdUnshare, "dashboard": cmdDashboard, "deletion": cmdDeletion,
	} {
		commands[k] = v
	}
}

// notSelfHosted: boat.dev verbs for its hosted account features (orgs,
// billing, repos, CI, credential profiles, retention, onboarding).
var notSelfHosted = []string{"org", "orgs", "billing", "repos", "repo", "ci", "credentials", "retention", "onboard", "signup"}

func compatVMPath(id string, rest ...string) string {
	p := "/api/v1/sandboxes/" + url.PathEscape(id)
	for _, r := range rest {
		p += "/" + r
	}
	return p
}

// cmdStatus prints a VM's state.
func cmdStatus(a *app, args []string) error {
	vm, err := oneVM(a, args)
	if err != nil {
		return err
	}
	v := obj{"id": vm["id"], "name": vm["name"], "state": vm["state"]}
	return a.emit(v, func() { fmt.Fprintf(a.out, "%s\t%s\t%s\n", s(vm, "id"), s(vm, "name"), s(vm, "state")) })
}

// cmdEvents lists a VM's agent events (prompts and their answers).
func cmdEvents(a *app, args []string) error {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	limit := fs.Int("limit", 50, "")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	id, err := oneVMID(a, pos)
	if err != nil {
		return err
	}
	evs, err := listCall(a, compatVMPath(id, "events")+fmt.Sprintf("?limit=%d", *limit), "events")
	if err != nil {
		return err
	}
	if a.json {
		for _, e := range evs {
			a.emit(e, nil)
		}
		return nil
	}
	for _, e := range evs {
		at := ""
		if ms, ok := e["timestamp"].(float64); ok {
			at = time.UnixMilli(int64(ms)).UTC().Format(time.RFC3339)
		}
		d, _ := e["data"].(map[string]any)
		text := fmt.Sprint(orAny(d["prompt"], d["text"], d["status"]))
		fmt.Fprintf(a.out, "%s\t%s\t%s\n", at, s(e, "type"), strings.TrimSpace(text))
	}
	return nil
}

func orAny(vs ...any) any {
	for _, v := range vs {
		if v != nil && v != "" {
			return v
		}
	}
	return ""
}

// postVM POSTs to a /api/v1 VM verb and prints its message (or the JSON).
func postVM(a *app, args []string, verb, done string) error {
	id, err := oneVMID(a, args)
	if err != nil {
		return err
	}
	var out obj
	if err := a.c.call("POST", compatVMPath(id, verb), nil, &out); err != nil {
		return err
	}
	return a.okLine(out, orStr(s(out, "message"), done+" "+id))
}

func cmdRecover(a *app, args []string) error { return postVM(a, args, "recover", "recovering") }
func cmdShare(a *app, args []string) error {
	return postVM(a, args, "share", "shared with the team (every sandbox here already is):")
}
func cmdUnshare(a *app, args []string) error { return postVM(a, args, "unshare", "private:") }

// cmdDashboard prints the web dashboard's URL (the API's origin) and opens it.
func cmdDashboard(a *app, args []string) error {
	fs := flag.NewFlagSet("dashboard", flag.ContinueOnError)
	noOpen := fs.Bool("no-open", false, "")
	if _, _, err := parse(fs, args); err != nil {
		return err
	}
	u, err := url.Parse(a.cfg.URL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("the API URL %q is not a URL", a.cfg.URL)
	}
	d := (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/"}).String()
	if a.json || *noOpen {
		return a.emit(obj{"url": d}, func() { fmt.Fprintln(a.out, d) })
	}
	fmt.Fprintln(a.out, d)
	return a.open(d)
}

// cmdDeletion: deletion status <operation id> (the operation boat.dev's SDKs
// get from a delete).
func cmdDeletion(a *app, args []string) error {
	op, rest := sub(args, "", "status")
	if op != "status" || len(rest) != 1 {
		return fmt.Errorf("boat deletion status <operation id>: %w", errUsage)
	}
	var out obj
	if err := a.c.call("GET", "/api/v1/deletion-operations/"+url.PathEscape(rest[0]), nil, &out); err != nil {
		return err
	}
	o, _ := out["operation"].(map[string]any)
	if o == nil {
		return errors.New("the server answered no operation")
	}
	return a.emit(o, func() { fmt.Fprintf(a.out, "%s\t%s\t%s\n", s(o, "id"), s(o, "targetId"), s(o, "status")) })
}

func oneVMID(a *app, pos []string) (string, error) {
	vm, err := oneVM(a, pos)
	if err != nil {
		return "", err
	}
	return s(vm, "id"), nil
}
