package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Round 12, continued: boat ai profile/accounts, the boat env verbs beyond
// ls/new/show/set/rm, and boat snapshot latest|tree|pull|delete|share.
// Each maps onto /api/v1, so the server's answer (or its not_supported) is
// what the user sees.

const profilesPath = "/api/v1/credential-profiles"

// readValue is the value argument, or stdin when it is missing or "-".
func (a *app) readValue(pos []string, i int) (string, error) {
	if len(pos) > i && pos[i] != "-" {
		return pos[i], nil
	}
	b, err := io.ReadAll(io.LimitReader(a.in, 1<<20))
	return strings.TrimRight(string(b), "\r\n"), err
}

func cmdAIProfiles(a *app, op string, args []string) error {
	fs := flag.NewFlagSet("ai "+op, flag.ContinueOnError)
	profile := fs.String("profile", "default", "the credential profile")
	copyFrom := fs.String("copy-from", "", "")
	baseURL := fs.String("base-url", "", "")
	value := fs.String("value", "", "")
	fs.Bool("org-only", false, "accepted: profiles here belong to the team")
	var hit string
	refuse(fs, &hit, "a profile here belongs to the team; every member uses it", "everyone", "only=", "add=", "remove=")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := refusedErr(hit); err != nil {
		return err
	}
	var out obj
	acct := func(cred string) string {
		return profilesPath + "/" + url.PathEscape(*profile) + "/accounts/" + url.PathEscape(cred)
	}
	switch op {
	case "profiles":
		l, err := listCall(a, profilesPath, "profiles")
		if err != nil {
			return err
		}
		return a.emitList(l, "id", "name", "isDefault", "connected")
	case "accounts":
		if err := need(pos, 1, "<credential>"); err != nil {
			return err
		}
		l, err := listCall(a, acct(pos[0]), "accounts")
		if err != nil {
			return err
		}
		return a.emitList(l, "id", "label", "active")
	case "switch", "forget-account":
		if err := need(pos, 2, "<credential> <account>"); err != nil {
			return err
		}
		id, err := a.accountID(acct(pos[0]), pos[1])
		if err != nil {
			return err
		}
		if op == "switch" {
			err = a.c.call("PUT", acct(pos[0])+"/"+url.PathEscape(id)+"/active", nil, &out)
		} else {
			err = a.c.call("DELETE", acct(pos[0])+"/"+url.PathEscape(id), nil, &out)
		}
		if err != nil {
			return err
		}
		return a.okLine(out, op+": "+pos[0]+" "+id)
	case "add-account":
		if len(pos) < 1 || len(pos) > 2 {
			return fmt.Errorf("boat ai add-account <credential> [value|-]: %w", errUsage)
		}
		v, err := a.readValue(pos, 1)
		if err != nil {
			return err
		}
		if err := a.c.call("POST", acct(pos[0]), obj{"value": v}, &out); err != nil {
			return err
		}
		return a.okLine(out, "added an account to "+pos[0])
	}
	// boat ai profile new|default|rename|rm|set-key|rm-key|set-api|rm-api|access
	if len(pos) == 0 {
		return fmt.Errorf("boat ai profile new|default|rename|rm|set-key|rm-key|set-api|rm-api <profile> ...: %w", errUsage)
	}
	verb, pos := pos[0], pos[1:]
	if err := need(pos[:min(len(pos), 1)], 1, "<profile>"); err != nil {
		return err
	}
	p := profilesPath + "/" + url.PathEscape(pos[0])
	switch verb {
	case "new":
		err = a.c.call("POST", profilesPath, omitNil(obj{"name": pos[0], "copyFrom": orNil(*copyFrom)}), &out)
	case "default":
		err = a.c.call("PUT", p, obj{"isDefault": true}, &out)
	case "rename":
		if err := need(pos, 2, "<profile> <new-name>"); err != nil {
			return err
		}
		err = a.c.call("PUT", p, obj{"name": pos[1]}, &out)
	case "rm":
		err = a.c.call("DELETE", p, nil, &out)
	case "set-key":
		if len(pos) < 2 || len(pos) > 3 {
			return fmt.Errorf("boat ai profile set-key <profile> <credential> [value|-]: %w", errUsage)
		}
		v, verr := a.readValue(pos, 2)
		if verr != nil {
			return verr
		}
		err = a.c.call("PUT", p+"/keys/"+url.PathEscape(pos[1]), obj{"value": v}, &out)
	case "rm-key":
		if err := need(pos, 2, "<profile> <credential>"); err != nil {
			return err
		}
		err = a.c.call("DELETE", p+"/keys/"+url.PathEscape(pos[1]), nil, &out)
	case "set-api", "rm-api":
		if err := need(pos, 2, "<profile> <NAME>"); err != nil {
			return err
		}
		method, body := "DELETE", any(nil)
		if verb == "set-api" {
			method, body = "PUT", obj{"baseUrl": *baseURL, "value": *value}
		}
		err = a.c.call(method, p+"/custom-apis/"+url.PathEscape(pos[1]), body, &out)
	case "access":
		return errors.New("boat ai profile access is not supported on self-hosted boat: a profile belongs to the team and every member uses it")
	default:
		return fmt.Errorf("unknown ai profile command %q: %w", verb, errUsage)
	}
	if err != nil {
		return err
	}
	return a.okLine(out, "ok: ai profile "+verb+" "+pos[0])
}

func orNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// accountID resolves boat.dev's account reference: its number (1-based), or
// an id or label as given.
func (a *app) accountID(listPath, ref string) (string, error) {
	n, err := strconv.Atoi(ref)
	if err != nil {
		return ref, nil
	}
	l, err := listCall(a, listPath, "accounts")
	if err != nil {
		return "", err
	}
	if n < 1 || n > len(l) {
		return "", fmt.Errorf("no account %d (there are %d)", n, len(l))
	}
	return s(l[n-1], "id"), nil
}

// cmdEnvMore: boat.dev's env verbs on /api/v1/environments (by name or id).
func cmdEnvMore(a *app, op string, args []string) error {
	fs := flag.NewFlagSet("env "+op, flag.ContinueOnError)
	from := fs.String("from", "", "read the file's contents from this local file (default: stdin)")
	branch := fs.String("branch", "", "")
	var hit string
	refuse(fs, &hit, "a self-hosted server has one team per key, no organizations", "org=")
	refuse(fs, &hit, "every member of the team can use every environment", "everyone", "only=", "add=", "remove=")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := refusedErr(hit); err != nil {
		return err
	}
	if op == "access" {
		return errors.New("boat env access is not supported on self-hosted boat: every member of the team can use every environment")
	}
	if err := need(pos[:min(len(pos), 1)], 1, "<env>"); err != nil {
		return err
	}
	p := "/api/v1/environments/" + url.PathEscape(pos[0])
	var out obj
	switch op {
	case "rename":
		if err := need(pos, 2, "<env> <new-name>"); err != nil {
			return err
		}
		err = a.c.call("PUT", p, obj{"name": pos[1]}, &out)
	case "set-var":
		if len(pos) != 2 {
			return fmt.Errorf("boat env set-var <env> KEY=VALUE (or KEY, value on stdin): %w", errUsage)
		}
		k, v, ok := strings.Cut(pos[1], "=")
		if !ok {
			if v, err = a.readValue(nil, 0); err != nil {
				return err
			}
		}
		err = a.c.call("PUT", p+"/vars/"+url.PathEscape(k), obj{"value": v}, &out)
	case "rm-var":
		if err := need(pos, 2, "<env> KEY"); err != nil {
			return err
		}
		err = a.c.call("DELETE", p+"/vars/"+url.PathEscape(pos[1]), nil, &out)
	case "set-file", "rm-file":
		if err := need(pos, 2, "<env> <path>"); err != nil {
			return err
		}
		if op == "rm-file" {
			err = a.c.call("DELETE", p+"/secret-files?path="+url.QueryEscape(pos[1]), nil, &out)
			break
		}
		var b []byte
		if *from != "" {
			b, err = os.ReadFile(*from)
		} else {
			b, err = io.ReadAll(io.LimitReader(a.in, 1<<20))
		}
		if err == nil {
			err = a.c.call("PUT", p+"/secret-files", obj{"path": pos[1], "contents": string(b)}, &out)
		}
	case "add-repo", "rm-repo":
		if err := need(pos, 2, "<env> owner/repo"); err != nil {
			return err
		}
		if op == "add-repo" {
			err = a.c.call("POST", p+"/repos", omitNil(obj{"repository": pos[1], "branch": orNil(*branch)}), &out)
		} else {
			err = a.c.call("DELETE", p+"/repos/"+url.PathEscape(pos[1]), nil, &out)
		}
	case "upgrade":
		err = a.c.call("POST", p+"/upgrade", nil, &out)
	}
	if err != nil {
		return err
	}
	return a.okLine(out, "ok: env "+op+" "+pos[0])
}

// cmdSnapshotMore: boat snapshot latest|tree|pull|delete|share|shares|unshare.
func cmdSnapshotMore(a *app, op string, args []string) error {
	fs := flag.NewFlagSet("snapshot "+op, flag.ContinueOnError)
	outDir := fs.String("o", "", "directory to write into")
	fs.StringVar(outDir, "out", "", "same as -o")
	all := fs.Bool("all", false, "delete every snapshot of this VM")
	fs.Bool("yes", false, "accepted: delete never asks here")
	fs.Bool("y", false, "same as --yes")
	var paths multi
	fs.Var(&paths, "path", "extract only this path (repeatable)")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	var out obj
	switch op {
	case "latest":
		id, err := oneVMID(a, pos)
		if err != nil {
			return err
		}
		if err := a.c.call("GET", compatVMPath(id, "snapshots", "latest"), nil, &out); err != nil {
			return err
		}
		sn, _ := out["snapshot"].(map[string]any)
		return a.show(obj(sn), "id", "status", "createdAt", "sizeBytes")
	case "tree":
		if err := need(pos, 1, "<snapshot-id>"); err != nil {
			return err
		}
		return a.c.call("GET", "/api/v1/snapshots/"+url.PathEscape(pos[0])+"/tree", nil, &out)
	case "pull":
		if err := need(pos, 1, "<snapshot-id>"); err != nil {
			return err
		}
		if len(paths) > 0 {
			return errors.New("--path is not supported on self-hosted boat: pull downloads the whole snapshot archive")
		}
		return a.pullSnapshot(pos[0], *outDir)
	case "delete":
		if *all {
			id, err := oneVMID(a, pos)
			if err != nil {
				return err
			}
			if err := a.c.call("DELETE", compatVMPath(id, "snapshots"), nil, &out); err != nil {
				return err
			}
			return a.okLine(out, "deleted the snapshots of "+id)
		}
		if len(pos) == 0 {
			return fmt.Errorf("boat snapshot delete SNAPSHOT_ID... | --all <vm>: %w", errUsage)
		}
		for _, sid := range pos {
			if err := a.c.call("DELETE", "/api/v1/snapshots/"+url.PathEscape(sid), nil, nil); err != nil {
				return fmt.Errorf("%s: %w", sid, err)
			}
			if !a.json {
				fmt.Fprintln(a.out, "deleted snapshot", sid)
			}
		}
		if a.json {
			return a.emit(obj{"ok": true, "deleted": pos}, nil)
		}
		return nil
	}
	// share|shares|unshare NAME: codes for a saved snapshot
	if err := need(pos[:min(len(pos), 1)], 1, "NAME"); err != nil {
		return err
	}
	p := "/api/v1/saved-snapshots/" + url.PathEscape(pos[0]) + "/shares"
	switch op {
	case "share":
		if err := a.c.call("POST", p, obj{}, &out); err != nil {
			return err
		}
		return a.okLine(out, s(out, "shareCode")+"\n(the code shows once; deploy with boat new --from CODE)")
	case "shares":
		l, err := listCall(a, p, "shares")
		if err != nil {
			return err
		}
		return a.emitList(l, "id", "status", "useCount", "createdAt")
	default:
		if err := need(pos, 2, "NAME SHARE_ID"); err != nil {
			return err
		}
		if err := a.c.call("DELETE", p+"/"+url.PathEscape(pos[1]), nil, &out); err != nil {
			return err
		}
		return a.okLine(out, "revoked "+pos[1])
	}
}

// pullSnapshot downloads the snapshot's one chunk (this server's archive)
// into dir.
func (a *app) pullSnapshot(sid, dir string) error {
	var d struct {
		Chunks []obj `json:"chunks"`
	}
	if err := a.c.call("GET", "/api/v1/snapshots/"+url.PathEscape(sid)+"/download", nil, &d); err != nil {
		return err
	}
	if len(d.Chunks) != 1 || s(d.Chunks[0], "signedUrl") == "" {
		return errors.New("the server answered no download chunk")
	}
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	resp, err := a.c.hc.Get(s(d.Chunks[0], "signedUrl")) // signed: no API key
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	dst := filepath.Join(dir, sid+".tar.zst")
	n, err := writeFile(dst, resp.Body)
	if err != nil {
		return err
	}
	return a.emit(obj{"ok": true, "path": dst, "bytes": n}, func() {
		fmt.Fprintf(a.out, "%s (%d bytes): a PandaOS snapshot archive; boat snapshots import %s restores it\n", dst, n, dst)
	})
}
