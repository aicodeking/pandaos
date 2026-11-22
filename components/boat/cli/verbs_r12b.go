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
	"runtime"
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
	pos, after, err := parse(fs, args)
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
		if len(after) > 0 || strings.TrimSpace(pos[0]) == "" {
			return fmt.Errorf("expected <env> [VM...]: %w", errUsage)
		}
		for _, ref := range pos[1:] {
			if strings.TrimSpace(ref) == "" {
				return fmt.Errorf("expected nonempty sandbox IDs or names: %w", errUsage)
			}
		}
		var body any
		if len(pos) > 1 {
			ids := make([]string, 0, len(pos)-1)
			for _, ref := range pos[1:] {
				vm, err := a.c.resolveVM(ref)
				if err != nil {
					return err
				}
				ids = append(ids, s(vm, "id"))
			}
			body = obj{"agentIds": ids}
		}
		if err := a.c.call("POST", p+"/upgrade", body, &out); err != nil {
			return err
		}
		summary := fmt.Sprintf("env %s: %s upgraded (%s live, %s stopped), %s failed", pos[0],
			numStr(out, "upgraded"), numStr(out, "upgradedLive"), numStr(out, "upgradedStopped"), numStr(out, "failed"))
		if err := a.okLine(out, summary); err != nil {
			return err
		}
		if failed, _ := out["failed"].(float64); failed > 0 || out["success"] == false {
			return fmt.Errorf("environment upgrade incomplete: %s", summary)
		}
		return nil
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
	yes := fs.Bool("yes", false, "confirm deletion (required with --json or without a terminal)")
	fs.BoolVar(yes, "y", false, "same as --yes")
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
		if sn == nil {
			return a.emit(nil, func() { fmt.Fprintln(a.out, "no snapshots") })
		}
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
			if err := need(pos, 1, "<vm>"); err != nil {
				return err
			}
			if err := a.requireConfirmFlag(*yes); err != nil {
				return err
			}
			id, err := oneVMID(a, pos)
			if err != nil {
				return err
			}
			if err := a.confirmDestroy(*yes, "all snapshots of "+id); err != nil {
				return err
			}
			if err := a.c.callH("DELETE", compatVMPath(id, "snapshots"), nil, &out, map[string]string{"X-Ascii-Confirm-Delete": id}); err != nil {
				return err
			}
			return a.okLine(out, "deleted the snapshots of "+id)
		}
		if len(pos) == 0 {
			return fmt.Errorf("boat snapshot delete SNAPSHOT_ID... | --all <vm>: %w", errUsage)
		}
		target := "snapshot " + pos[0]
		if len(pos) > 1 {
			target = fmt.Sprintf("%d snapshots (%s)", len(pos), strings.Join(pos, ", "))
		}
		if err := a.confirmDestroy(*yes, target); err != nil {
			return err
		}
		for _, sid := range pos {
			if err := a.c.callH("DELETE", "/api/v1/snapshots/"+url.PathEscape(sid), nil, nil, map[string]string{"X-Ascii-Confirm-Delete": sid}); err != nil {
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
	if filepath.Base(sid) != sid || !validSSHConfigID(sid) {
		return fmt.Errorf("invalid snapshot id %q", sid)
	}
	var d struct {
		Chunks []obj `json:"chunks"`
	}
	if err := a.c.call("GET", "/api/v1/snapshots/"+url.PathEscape(sid)+"/download", nil, &d); err != nil {
		return err
	}
	if len(d.Chunks) != 1 || s(d.Chunks[0], "signedUrl") == "" {
		return errors.New("the server answered no download chunk")
	}
	signedURL := s(d.Chunks[0], "signedUrl")
	baseURL := a.c.base
	if baseURL == "" {
		baseURL = a.cfg.URL
	}
	if err := validateSnapshotDownloadURL(signedURL, baseURL); err != nil {
		return err
	}
	hc := *a.c.hc
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return validateSnapshotDownloadURL(req.URL.String(), baseURL)
	}
	resp, err := hc.Get(signedURL) // signed: no API key
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(dir, sid+".tar.zst")
	n, err := writePrivateFile(dst, resp.Body)
	if err != nil {
		return err
	}
	return a.emit(obj{"ok": true, "path": dst, "bytes": n}, func() {
		fmt.Fprintf(a.out, "%s (%d bytes): a PandaOS snapshot archive; boat snapshots import %s restores it\n", dst, n, dst)
	})
}

func validateSnapshotDownloadURL(rawURL, baseURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("refusing snapshot download URL %q: invalid URL", rawURL)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		bu, berr := url.Parse(baseURL)
		if berr != nil || strings.ToLower(bu.Scheme) != "http" || bu.Host == "" || bu.Hostname() == "" {
			return fmt.Errorf("refusing insecure snapshot download URL %q: must use https (or http matching configured BOAT_URL origin)", rawURL)
		}
		if !sameOrigin(u, bu) {
			return fmt.Errorf("refusing insecure snapshot download URL %q: http origin %q does not match configured BOAT_URL origin %q", rawURL, urlOrigin(u), urlOrigin(bu))
		}
		return nil
	default:
		return fmt.Errorf("refusing snapshot download URL %q: unsupported scheme %q (must be https)", rawURL, u.Scheme)
	}
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	}
	return ""
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectivePort(a) == effectivePort(b)
}

func urlOrigin(u *url.URL) string {
	return strings.ToLower(u.Scheme) + "://" + u.Host
}

func writePrivateFile(dst string, r io.Reader) (int64, error) {
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) // may hold secrets
	if err != nil {
		return 0, err
	}
	if runtime.GOOS != "windows" {
		if err := f.Chmod(0o600); err != nil {
			f.Close()
			os.Remove(dst)
			return 0, err
		}
	}
	n, err := io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
		return 0, err
	}
	return n, nil
}
