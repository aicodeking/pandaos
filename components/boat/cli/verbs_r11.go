package main

import (
	"fmt"
	"slices"
	"strings"
)

func init() {
	for k, v := range map[string]func(*app, []string) error{
		"ssh-keys": cmdSSHKeys, "browser": cmdDesktop, "convos": cmdConversations,
	} {
		commands[k] = v
	}
}

const feedbackText = `boat (self-hosted) is part of PandaOS.
File bugs and feature requests at https://github.com/aicodeking/pandaos/issues
(include boat version, the command, and the X-Request-Id from the error if any).
Server problems: ask your boat operator; boat.dev's own feedback channel does not reach this server.`

// verbUsage: the usage lines that mention "boat <cmd>", or all of usage.
func verbUsage(cmd string) string {
	var out []string
	for _, l := range strings.Split(usage, "\n") {
		for _, f := range strings.Split(l, "boat ")[1:] {
			name, _, _ := strings.Cut(f, " ")
			if slices.Contains(strings.Split(name, "|"), cmd) {
				out = append(out, l)
				break
			}
		}
	}
	if len(out) == 0 {
		return usage
	}
	return strings.Join(out, "\n")
}

// wantsHelp: --help/-h before any "--" (a remote command's own flags stay its own).
func wantsHelp(args []string) bool {
	for _, x := range args {
		if x == "--" {
			return false
		}
		if x == "--help" || x == "-h" || x == "-help" {
			return true
		}
	}
	return false
}

// cmdSSHKeys lists or revokes the ssh keys granted on a VM.
func cmdSSHKeys(a *app, args []string) error {
	op, args := sub(args, "ls", "ls", "rm")
	want, what := 1, "VM"
	if op == "rm" {
		want, what = 2, "VM FINGERPRINT"
	}
	if err := need(args, want, what); err != nil {
		return err
	}
	id, err := vmArg(a, args[0])
	if err != nil {
		return err
	}
	if op == "ls" {
		l, err := listCall(a, vmPath(id, "ssh"), "keys")
		if err != nil {
			return err
		}
		for _, k := range l {
			if k["unowned"] == true {
				k["user_id"] = "unowned"
			}
		}
		return a.emitList(l, "fingerprint", "user_id", "key_id", "public_key")
	}
	// URL-safe form: proxies may refuse or decode %2F.
	fp := strings.NewReplacer("+", "-", "/", "_").Replace(strings.TrimPrefix(args[1], "SHA256:"))
	var out obj
	if err := a.c.call("DELETE", vmPath(id, "ssh", fp), nil, &out); err != nil {
		return err
	}
	return a.emit(out, func() { fmt.Fprintln(a.out, "revoked", s(out, "fingerprint")) })
}
