//go:build linux

// Command testagent is a minimal stand-in for guest/agent/boat-agent.py, used
// by e2e.sh only when the test rootfs has no usable python3. It speaks the same
// protocol (../../../PROTOCOL.md) for health, exec (no tty), set_identity,
// set_env, put_file/get_file and shutdown. Not for production images.
package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const afVsock = 40

type sockaddrVM struct {
	family    uint16
	reserved1 uint16
	port      uint32
	cid       uint32
	flags     uint8
	zero      [3]uint8
}

func listen(port uint32) (int, error) {
	fd, err := syscall.Socket(afVsock, syscall.SOCK_STREAM, 0)
	if err != nil {
		return -1, err
	}
	sa := sockaddrVM{family: afVsock, port: port, cid: 0xFFFFFFFF} // VMADDR_CID_ANY
	if _, _, e := syscall.Syscall(syscall.SYS_BIND, uintptr(fd), uintptr(unsafe.Pointer(&sa)), unsafe.Sizeof(sa)); e != 0 {
		return -1, e
	}
	return fd, syscall.Listen(fd, 64)
}

var started = time.Now()

func main() {
	fd, err := listen(1024)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testagent:", err)
		os.Exit(1)
	}
	for {
		nfd, _, e := syscall.Syscall6(syscall.SYS_ACCEPT4, uintptr(fd), 0, 0, 0, 0, 0)
		if e != 0 {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		go serve(os.NewFile(nfd, "vsock"))
	}
}

func serve(c *os.File) {
	defer c.Close()
	r := bufio.NewReaderSize(c, 1<<20)
	send := func(v any) { b, _ := json.Marshal(v); c.Write(append(b, '\n')) }
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var q map[string]any
		if json.Unmarshal(line, &q) != nil {
			send(map[string]string{"error": "bad json"})
			continue
		}
		if err := handle(q, send); err != nil {
			send(map[string]string{"error": err.Error()})
		}
	}
}

func s(q map[string]any, k string) string { v, _ := q[k].(string); return v }

func run(argv ...string) error {
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %v: %s", strings.Join(argv, " "), err, out)
	}
	return nil
}

func handle(q map[string]any, send func(any)) error {
	switch s(q, "op") {
	case "health":
		send(map[string]any{"ok": true, "uptime": time.Since(started).Seconds(), "idle_s": 0, "version": "test"})
	case "exec":
		var argv []string
		for _, a := range q["argv"].([]any) {
			argv = append(argv, a.(string))
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = s(q, "cwd")
		cmd.Env = os.Environ()
		if env, ok := q["env"].(map[string]any); ok {
			for k, v := range env {
				cmd.Env = append(cmd.Env, k+"="+fmt.Sprint(v))
			}
		}
		if in := s(q, "stdin_b64"); in != "" {
			b, _ := base64.StdEncoding.DecodeString(in)
			cmd.Stdin = strings.NewReader(string(b))
		}
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		send(map[string]any{"stream": "stdout", "data_b64": base64.StdEncoding.EncodeToString(out)})
		if stderr.Len() > 0 {
			send(map[string]any{"stream": "stderr", "data_b64": base64.StdEncoding.EncodeToString([]byte(stderr.String()))})
		}
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			return err
		}
		send(map[string]any{"exit": code})
	case "set_identity":
		mac, cidr, gw := s(q, "mac"), s(q, "ipv4_cidr"), s(q, "gw")
		steps := [][]string{{"ip", "link", "set", "eth0", "down"}}
		if mac != "" {
			steps = append(steps, []string{"ip", "link", "set", "eth0", "address", mac})
		}
		steps = append(steps, []string{"ip", "link", "set", "eth0", "up"}, []string{"ip", "-4", "addr", "flush", "dev", "eth0"},
			[]string{"ip", "addr", "add", cidr, "dev", "eth0"}, []string{"ip", "route", "replace", "default", "via", gw},
			[]string{"hostname", s(q, "hostname")}, []string{"rm", "-f", "/etc/machine-id"}, []string{"systemd-machine-id-setup"},
			[]string{"sh", "-c", "rm -f /etc/ssh/ssh_host_* && ssh-keygen -A && (systemctl try-restart ssh || true)"})
		for _, st := range steps {
			if err := run(st...); err != nil {
				return err
			}
		}
		send(map[string]bool{"ok": true})
	case "set_env":
		var b strings.Builder
		for k, v := range q["vars"].(map[string]any) {
			fmt.Fprintf(&b, "export %s='%s'\n", k, strings.ReplaceAll(fmt.Sprint(v), "'", `'\''`))
		}
		os.MkdirAll("/etc/boat", 0o755)
		if err := os.WriteFile("/etc/boat/env", []byte(b.String()), 0o600); err != nil {
			return err
		}
		send(map[string]bool{"ok": true})
	case "put_file":
		data, _ := base64.StdEncoding.DecodeString(s(q, "data_b64"))
		flags := os.O_WRONLY | os.O_CREATE | os.O_APPEND
		if off, _ := q["offset"].(float64); off == 0 {
			flags |= os.O_TRUNC
		}
		f, err := os.OpenFile(s(q, "path"), flags, 0o644)
		if err != nil {
			return err
		}
		f.Write(data)
		f.Close()
		send(map[string]any{"ok": true, "size": len(data)})
	case "get_file":
		b, err := os.ReadFile(s(q, "path"))
		if err != nil {
			return err // "...: no such file or directory"
		}
		send(map[string]any{"offset": 0, "data_b64": base64.StdEncoding.EncodeToString(b)})
		send(map[string]any{"eof": true, "size": len(b)})
	case "shutdown":
		send(map[string]bool{"ok": true})
		go func() {
			time.Sleep(500 * time.Millisecond)
			syscall.Sync()
			syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
		}()
	default:
		return fmt.Errorf("unknown op: %q", s(q, "op"))
	}
	return nil
}
