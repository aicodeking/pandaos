package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

const (
	maxArgLen  = 4 << 10
	maxArgvLen = 64 << 10
)

var jobIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// checkRelayURL requires https. Plain http is allowed only with allowInsecure,
// and only to hosts that cannot be on the public internet: loopback,
// private/link-local IPs, `.local` names, and single-label names.
func checkRelayURL(raw string, allowInsecure bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("invalid relay URL %q: want https://host[:port]", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if !localHost(u.Hostname()) {
			return fmt.Errorf("relay URL %q: http is only allowed to loopback, private or .local hosts; use https", raw)
		}
		if !allowInsecure {
			return fmt.Errorf("relay URL %q: http needs -allow-insecure", raw)
		}
		return nil
	}
	return fmt.Errorf("invalid relay URL %q: scheme must be https", raw)
}

func localHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") || (host != "" && !strings.Contains(host, "."))
}

// validateJob rejects anything that could escape the workdir path or bloat a
// systemd-run command line.
func validateJob(j Job) error {
	if !jobIDRe.MatchString(j.ID) {
		return errors.New("invalid job id: want [A-Za-z0-9_-]{1,64}")
	}
	if len(j.Argv) == 0 || j.Argv[0] == "" {
		return errors.New("empty argv")
	}
	total := 0
	for i, a := range j.Argv {
		if strings.IndexByte(a, 0) >= 0 {
			return fmt.Errorf("argv[%d] contains NUL", i)
		}
		if len(a) > maxArgLen {
			return fmt.Errorf("argv[%d] longer than %d bytes", i, maxArgLen)
		}
		total += len(a)
	}
	if total > maxArgvLen {
		return fmt.Errorf("argv longer than %d bytes in total", maxArgvLen)
	}
	return nil
}
