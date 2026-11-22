package main

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

var (
	sshUserRe       = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)
	sshHostRe       = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	sshConfigIDRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	sshConfigSlugRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

func validSSHUser(u string) bool       { return sshUserRe.MatchString(u) }
func validSSHConfigID(id string) bool  { return sshConfigIDRe.MatchString(id) }
func validSSHConfigSlug(n string) bool { return sshConfigSlugRe.MatchString(n) }

// validSSHHost accepts a hostname or an IP address (no brackets).
func validSSHHost(h string) bool {
	if net.ParseIP(h) != nil {
		return true
	}
	return sshHostRe.MatchString(h)
}

func validSSHPort(p string) bool {
	n, err := strconv.Atoi(p)
	return err == nil && n >= 1 && n <= 65535 && strconv.Itoa(n) == p
}

// parseSSHJump parses user@host or user@host:port (host may be [ipv6] or bare ipv6).
// An unbracketed host containing more than one ":" is an IPv6 literal with port 22.
func parseSSHJump(j string) (user, host, port string, ok bool) {
	at := strings.Index(j, "@")
	if at < 0 || !validSSHUser(j[:at]) {
		return "", "", "", false
	}
	user = j[:at]
	rest := j[at+1:]
	port = "22"
	if strings.HasPrefix(rest, "[") {
		end := strings.Index(rest, "]")
		if end < 0 {
			return "", "", "", false
		}
		host = rest[1:end]
		if ip := net.ParseIP(host); ip == nil || !strings.Contains(host, ":") {
			return "", "", "", false
		}
		tail := rest[end+1:]
		if tail != "" {
			if !strings.HasPrefix(tail, ":") {
				return "", "", "", false
			}
			port = tail[1:]
			if !validSSHPort(port) {
				return "", "", "", false
			}
		}
		return user, host, port, true
	}
	switch strings.Count(rest, ":") {
	case 0:
		host = rest
	case 1:
		i := strings.Index(rest, ":")
		host, port = rest[:i], rest[i+1:]
		if !validSSHPort(port) {
			return "", "", "", false
		}
	default:
		// Unbracketed IPv6 literal; a custom port requires brackets.
		host = rest
		if net.ParseIP(host) == nil {
			return "", "", "", false
		}
		return user, host, port, true
	}
	if !validSSHHost(host) {
		return "", "", "", false
	}
	return user, host, port, true
}

// validSSHJump accepts user@host or user@host:port (host may be [ipv6] or bare ipv6).
func validSSHJump(j string) bool {
	_, _, _, ok := parseSSHJump(j)
	return ok
}

// validateSSHInfo refuses ssh target fields from the API that could inject
// options or shell syntax into the ssh argv or ProxyCommand.
func validateSSHInfo(info obj) error {
	if u := s(info, "user"); !validSSHUser(u) {
		return fmt.Errorf("server returned an invalid ssh user %q", u)
	}
	if h := s(info, "host"); !validSSHHost(h) {
		return fmt.Errorf("server returned an invalid ssh host %q", h)
	}
	if p := s(info, "port"); !validSSHPort(p) {
		return fmt.Errorf("server returned an invalid ssh port %q", p)
	}
	if j := s(info, "jump"); j != "" && !validSSHJump(j) {
		return fmt.Errorf("server returned an invalid ssh jump host %q", j)
	}
	return nil
}
