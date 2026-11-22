#!/usr/bin/env bash
# Bring up boatd + boat-api from source on a Linux x86_64 host with /dev/kvm
# (a GitHub ubuntu runner) for boatbench, with boatd's PRODUCTION defaults
# (sizes default = 4 vCPU / 8 GiB / 51 GiB, diff snapshots, jailer, disk
# limits, no egress cap) except the overcommit needed to fit 10 x 8 GiB VMs on
# a 16 GiB runner.
#
#   stack.sh up GUEST_DIR   # GUEST_DIR has vmlinux, rootfs-slim.ext4; writes $WORK/env (BOAT_URL, BOAT_KEY)
#   BOATBENCH_PRESET=untrusted stack.sh up ...   # presets.json: trusted (default), trusted-warm, trusted-writeback, untrusted
#   BOATBENCH_BOATD_OPTIONS='{"warmPool":true}' stack.sh up ...   # JSON merged over boatd.json and the preset
#   BENCH_WORK=/opt/boatbench/work BENCH_STATE=/opt/boatbench/state stack.sh up ...   # a real server: all under one dir
#   stack.sh down
#
# ponytail: components/boat/e2e.sh has no "stack only" mode and is outside this
# benchmark's ownership, so this mirrors its setup steps (the Firecracker
# version and checksum are READ from e2e.sh, not copied). If e2e.sh grows an
# E2E_STACK_ONLY switch, call it here instead.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
BOAT=$ROOT/components/boat
WORK=${BENCH_WORK:-/mnt/boatbench}
STATE=${BENCH_STATE:-/var/lib/boatbench}   # a real server: keep it under $WORK (BENCH_STATE=$WORK/state)
SOCK=$WORK/run/boatd.sock
API=http://127.0.0.1:8740
HTTPS_HOST=${BENCH_HTTPS_HOST:-api.boat.test}
HTTPS_PORT=${BENCH_HTTPS_PORT:-8443}
# shellcheck disable=SC2016 # literal ${...} in the sed pattern
FC_VERSION=$(sed -n 's/^FC_VERSION=\${FC_VERSION:-\(v[0-9.]*\)}.*/\1/p' "$BOAT/e2e.sh")
FC_SHA256=$(sed -n 's/^FC_SHA256=\([0-9a-f]\{64\}\).*/\1/p' "$BOAT/e2e.sh")
log() { printf '\n\033[1m==> %s\033[0m\n' "$*" >&2; }

stop_pid() {
  local f=$1 as_root=${2:-} pid
  [ -f "$f" ] || return 0
  pid=$(tr -d '[:space:]' <"$f" 2>/dev/null) || pid=
  if [[ "$pid" =~ ^[0-9]+$ ]] && [ "$pid" -gt 1 ]; then
    if [ "$as_root" = root ]; then
      sudo kill "$pid" 2>/dev/null || true
      for _ in 1 2 3 4 5 6 7 8 9 10; do sudo kill -0 "$pid" 2>/dev/null || break; sleep 0.1; done
      sudo kill -9 "$pid" 2>/dev/null || true
    else
      kill "$pid" 2>/dev/null || true
      for _ in 1 2 3 4 5 6 7 8 9 10; do kill -0 "$pid" 2>/dev/null || break; sleep 0.1; done
      kill -9 "$pid" 2>/dev/null || true
    fi
  fi
  if [ "$as_root" = root ]; then
    sudo rm -f "$f" 2>/dev/null || true
  else
    rm -f "$f" 2>/dev/null || true
  fi
}

write_tls_proxy() {
  local dir=$1
  mkdir -p "$dir"
  cat >"$dir/go.mod" <<'GO'
module boatbench-tls

go 1.24
GO
  cat >"$dir/main.go" <<'GO'
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const maxBufferedBody = 1 << 20

var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func main() {
	initOnly := flag.Bool("init", false, "ensure local CA + server cert exist and print bridge gateway IP")
	dir := flag.String("dir", "", "certificate directory")
	host := flag.String("host", "api.boat.test", "TLS server hostname")
	subnet := flag.String("subnet", "10.200.0.0/16", "boatd bridge CIDR")
	listen := flag.String("listen", "10.200.0.1:8443", "HTTPS listen address")
	upstream := flag.String("upstream", "http://127.0.0.1:8740", "boat-api HTTP upstream")
	flag.Parse()

	if *dir == "" {
		log.Fatal("-dir is required")
	}
	if *initOnly {
		gw, err := gatewayForSubnet(*subnet)
		if err != nil {
			log.Fatalf("subnet %q: %v", *subnet, err)
		}
		if err := ensureCerts(*dir, *host, gw); err != nil {
			log.Fatalf("certs: %v", err)
		}
		fmt.Println(gw.String())
		return
	}
	if err := serveTLS(*dir, *listen, *upstream); err != nil {
		log.Fatal(err)
	}
}

func gatewayForSubnet(cidr string) (netip.Addr, error) {
	pfx, err := netip.ParsePrefix(strings.TrimSpace(cidr))
	if err != nil {
		return netip.Addr{}, err
	}
	gw := pfx.Masked().Addr().Next()
	if !gw.IsValid() || !gw.Is4() || !pfx.Contains(gw) {
		return netip.Addr{}, fmt.Errorf("invalid IPv4 bridge subnet %q", cidr)
	}
	return gw, nil
}

func ensureCerts(dir, host string, gw netip.Addr) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	caPath := filepath.Join(dir, "ca.pem")
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if certsValid(caPath, certPath, keyPath, host, gw) {
		return nil
	}

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	caSKI, err := subjectKeyID(&caKey.PublicKey)
	if err != nil {
		return err
	}
	caSerial, err := randomSerial()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	caTmpl := &x509.Certificate{
		SerialNumber:          caSerial,
		Subject:               pkix.Name{CommonName: "boatbench local CA"},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		SubjectKeyId:          caSKI,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return err
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	leafSKI, err := subjectKeyID(&leafKey.PublicKey)
	if err != nil {
		return err
	}
	leafSerial, err := randomSerial()
	if err != nil {
		return err
	}
	dnsNames := []string{host}
	if host != "localhost" {
		dnsNames = append(dnsNames, "localhost")
	}
	gwIP := net.IP(gw.AsSlice())
	ipAddrs := []net.IP{gwIP}
	if gw.String() != "127.0.0.1" {
		ipAddrs = append(ipAddrs, net.ParseIP("127.0.0.1"))
	}
	leafTmpl := &x509.Certificate{
		SerialNumber:   leafSerial,
		Subject:        pkix.Name{CommonName: host},
		NotBefore:      now.Add(-1 * time.Hour),
		NotAfter:       now.Add(365 * 24 * time.Hour),
		KeyUsage:       x509.KeyUsageDigitalSignature,
		ExtKeyUsage:    []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:       dnsNames,
		IPAddresses:    ipAddrs,
		SubjectKeyId:   leafSKI,
		AuthorityKeyId: caSKI,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	leafKeyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return err
	}
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), 0o644); err != nil {
		return err
	}
	return os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: leafKeyDER}), 0o600)
}

func certsValid(caPath, certPath, keyPath, host string, gw netip.Addr) bool {
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return false
	}
	block, _ := pem.Decode(caPEM)
	if block == nil {
		return false
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil || len(pair.Certificate) == 0 {
		return false
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return false
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:       pool,
		CurrentTime: time.Now().Add(24 * time.Hour),
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return false
	}
	for _, name := range []string{host, "localhost", gw.String(), "127.0.0.1"} {
		if leaf.VerifyHostname(name) != nil {
			return false
		}
	}
	return true
}

func subjectKeyID(pub *ecdsa.PublicKey) ([]byte, error) {
	spki, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(spki)
	return sum[:20], nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, limit)
}

func allowedAPIPath(rawPath, escapedPath string) bool {
	if !strings.HasPrefix(rawPath, "/") || strings.Contains(rawPath, "\\") {
		return false
	}
	lowerEscaped := strings.ToLower(escapedPath)
	if strings.Contains(lowerEscaped, "%2e") || strings.Contains(lowerEscaped, "%2f") || strings.Contains(lowerEscaped, "%5c") {
		return false
	}
	for _, seg := range strings.Split(rawPath, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	if rawPath == "/healthz" || rawPath == "/openapi.json" || rawPath == "/openapi.yaml" || rawPath == "/v1" || rawPath == "/api/v1" {
		return true
	}
	return strings.HasPrefix(rawPath, "/v1/") || strings.HasPrefix(rawPath, "/api/v1/")
}

func stripHopHeaders(h http.Header) {
	for _, connVal := range h.Values("Connection") {
		for _, tok := range strings.Split(connVal, ",") {
			if tok = strings.TrimSpace(tok); tok != "" {
				h.Del(tok)
			}
		}
	}
	for _, k := range hopByHopHeaders {
		h.Del(k)
	}
}

func serveTLS(dir, listenAddr, upstreamURL string) error {
	base, err := url.Parse(upstreamURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return fmt.Errorf("invalid -upstream %q", upstreamURL)
	}
	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	if err != nil {
		return err
	}
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   128,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		DisableCompression:    true,
	}
	client := &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowedAPIPath(r.URL.Path, r.URL.EscapedPath()) {
			body := []byte("not found\n")
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write(body)
			return
		}

		target := *r.URL
		target.Scheme = base.Scheme
		target.Host = base.Host
		outReq, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), r.Body)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		outReq.URL.Path = r.URL.Path
		outReq.URL.RawPath = r.URL.RawPath
		outReq.URL.RawQuery = r.URL.RawQuery
		outReq.ContentLength = r.ContentLength
		outReq.Host = r.Host
		outReq.Header = r.Header.Clone()
		stripHopHeaders(outReq.Header)
		for _, k := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip"} {
			outReq.Header.Del(k)
		}
		if remoteHost, _, splitErr := net.SplitHostPort(r.RemoteAddr); splitErr == nil && remoteHost != "" {
			outReq.Header.Set("X-Forwarded-For", remoteHost)
		} else if r.RemoteAddr != "" {
			outReq.Header.Set("X-Forwarded-For", r.RemoteAddr)
		}
		outReq.Header.Set("X-Forwarded-Proto", "https")
		if r.Host != "" {
			outReq.Header.Set("X-Forwarded-Host", r.Host)
		}

		resp, err := client.Do(outReq)
		if err != nil {
			body := []byte("bad gateway\n")
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write(body)
			return
		}
		defer resp.Body.Close()

		stripHopHeaders(resp.Header)
		for k, vals := range resp.Header {
			if strings.EqualFold(k, "Content-Length") {
				continue
			}
			for _, v := range vals {
				w.Header().Add(k, v)
			}
		}

		if resp.StatusCode < 200 || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified || r.Method == http.MethodHead {
			if r.Method == http.MethodHead && resp.ContentLength >= 0 {
				w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
			}
			w.WriteHeader(resp.StatusCode)
			return
		}

		isSSE := strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
		if !isSSE && resp.ContentLength >= 0 && resp.ContentLength <= maxBufferedBody {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBufferedBody+1))
			if readErr != nil || int64(len(body)) != resp.ContentLength {
				for k := range w.Header() {
					delete(w.Header(), k)
				}
				msg := []byte("bad gateway\n")
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.Header().Set("Content-Length", strconv.Itoa(len(msg)))
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write(msg)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(body)
			return
		}

		if resp.ContentLength > maxBufferedBody {
			w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
		}
		w.WriteHeader(resp.StatusCode)
		if flusher, ok := w.(http.Flusher); ok {
			buf := make([]byte, 32*1024)
			for {
				n, readErr := resp.Body.Read(buf)
				if n > 0 {
					if _, writeErr := w.Write(buf[:n]); writeErr != nil {
						return
					}
					flusher.Flush()
				}
				if readErr != nil {
					return
				}
			}
		}
		_, _ = io.Copy(w, resp.Body)
	})

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{pair},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"http/1.1"},
	}
	rawLn, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return err
	}
	ln := tls.NewListener(rawLn, tlsCfg)
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Printf("boatbench-tls listening on https://%s -> %s", listenAddr, upstreamURL)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
GO
}

down() {
  set +e
  sudo pkill -9 -x firecracker
  if command -v iptables >/dev/null; then
    while sudo iptables -C FORWARD -i boatbr0 -j ACCEPT 2>/dev/null; do sudo iptables -D FORWARD -i boatbr0 -j ACCEPT; done
    while sudo iptables -C FORWARD -o boatbr0 -j ACCEPT 2>/dev/null; do sudo iptables -D FORWARD -o boatbr0 -j ACCEPT; done
  fi
  stop_pid "$WORK/tls.pid"
  stop_pid "$WORK/api.pid"
  stop_pid "$WORK/boatd.pid" root
  for t in $(ip -o link show | awk -F': ' '{print $2}' | cut -d@ -f1 | grep -E '^bt[0-9]+$'); do sudo ip link del "$t"; done
  sudo ip link del boatbr0 2>/dev/null
  sudo nft delete table inet boat 2>/dev/null
  sudo nft delete table bridge boat 2>/dev/null
  mountpoint -q "$WORK/guest-ca-mount" && sudo umount -l "$WORK/guest-ca-mount"
  rmdir "$WORK/guest-ca-mount" 2>/dev/null
  mountpoint -q "$STATE" && sudo umount -l "$STATE"
  sudo rmdir /sys/fs/cgroup/boat-bench 2>/dev/null
  true
}

up() {
  local guest
  guest=$(cd "${1:?usage: stack.sh up GUEST_DIR}" && pwd)
  [ -e /dev/kvm ] || { echo "stack: /dev/kvm missing" >&2; exit 1; }
  [ -n "$FC_VERSION" ] && [ -n "$FC_SHA256" ] || { echo "stack: cannot read FC_VERSION/FC_SHA256 from e2e.sh" >&2; exit 1; }
  [ -f "$ROOT/components/boat/guest/ssh-authorize.py" ] || { echo "stack: missing $ROOT/components/boat/guest/ssh-authorize.py" >&2; exit 1; }
  sudo mkdir -p "$WORK" && sudo chown "$(id -u):$(id -g)" "$WORK"
  mkdir -p "$WORK/bin" "$WORK/dl" "$WORK/run" "$WORK/api" "$WORK/tls"

  log "tools + runner network (same fixes as e2e.sh: br_netfilter off, FORWARD accept)"
  local missing=()
  for c in jq:jq nft:nftables mkfs.btrfs:btrfs-progs curl:curl; do command -v "${c%%:*}" >/dev/null || missing+=("${c#*:}"); done
  [ ${#missing[@]} = 0 ] || { sudo apt-get update -qq && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "${missing[@]}" >/dev/null; }
  if lsmod | grep -q '^br_netfilter'; then
    sudo modprobe -r br_netfilter 2>/dev/null ||
      sudo sysctl -qw net.bridge.bridge-nf-call-iptables=0 net.bridge.bridge-nf-call-ip6tables=0 net.bridge.bridge-nf-call-arptables=0
  fi
  if command -v iptables >/dev/null; then
    sudo iptables -I FORWARD -i boatbr0 -j ACCEPT
    sudo iptables -I FORWARD -o boatbr0 -j ACCEPT
  fi

  log "firecracker $FC_VERSION (checksum from e2e.sh)"
  if [ ! -x "$WORK/dl/firecracker" ]; then
    curl -fsSLo "$WORK/dl/fc.tgz" "https://github.com/firecracker-microvm/firecracker/releases/download/$FC_VERSION/firecracker-$FC_VERSION-x86_64.tgz"
    echo "$FC_SHA256  $WORK/dl/fc.tgz" | sha256sum -c -
    tar -xzf "$WORK/dl/fc.tgz" -C "$WORK/dl"
    mv "$WORK/dl/release-$FC_VERSION-x86_64/firecracker-$FC_VERSION-x86_64" "$WORK/dl/firecracker"
    mv "$WORK/dl/release-$FC_VERSION-x86_64/jailer-$FC_VERSION-x86_64" "$WORK/dl/jailer"
  fi

  log "go build boatd, boat-api, boatbench-tls"
  (cd "$BOAT/boatd" && CGO_ENABLED=0 go build -o "$WORK/bin/boatd" .)
  (cd "$BOAT/api" && CGO_ENABLED=0 go build -o "$WORK/bin/boat-api" .)
  write_tls_proxy "$WORK/tls"
  (cd "$WORK/tls" && CGO_ENABLED=0 go build -o "$WORK/bin/boatbench-tls" .)

  log "boatd options: preset ${BOATBENCH_PRESET:-trusted}, then BOATBENCH_BOATD_OPTIONS"
  local preset=${BOATBENCH_PRESET:-trusted} options=${BOATBENCH_BOATD_OPTIONS:-'{}'} merged
  jq -e --arg p "$preset" 'has($p) and ($p | startswith("_") | not)' "$HERE/presets.json" >/dev/null ||
    { echo "stack: unknown BOATBENCH_PRESET $preset (see presets.json)" >&2; exit 2; }
  jq -e 'type == "object"' <<<"$options" >/dev/null || { echo "stack: BOATBENCH_BOATD_OPTIONS must be a JSON object: $options" >&2; exit 2; }
  merged=$(jq -c --arg p "$preset" --argjson o "$options" '.[$p] * $o' "$HERE/presets.json")
  local subnet https_port bridge_ip vm_api_url use_local_tls=true api_public_args=() public_addrs='[]'
  subnet=$(jq -r '.subnet // "10.200.0.0/16"' <<<"$merged")
  bridge_ip=$("$WORK/bin/boatbench-tls" -init -dir "$WORK/tls" -host "$HTTPS_HOST" -subnet "$subnet")
  if [[ "${PUBLIC_URL:-}" == https://* ]]; then
    use_local_tls=false
    vm_api_url=${PUBLIC_URL%/}
    api_public_args=(-public-url "$vm_api_url")
    local pub_default_port
    pub_default_port=$(python3 -c 'import sys,urllib.parse; print(urllib.parse.urlsplit(sys.argv[1]).port or 443)' "$vm_api_url")
    https_port=$(jq -r --argjson d "$pub_default_port" '.hostHttpsPort // $d' <<<"$merged")
    public_addrs=$(python3 -c '
import ipaddress, json, socket, subprocess, sys, urllib.parse
u = urllib.parse.urlsplit(sys.argv[1])
out = []
def add(s, require_global=False):
    try:
        ip = ipaddress.IPv4Address(s.strip())
    except ValueError:
        return
    if ip.is_unspecified or ip.is_loopback or ip.is_multicast or str(ip) == "255.255.255.255":
        return
    if require_global and not ip.is_global:
        return
    if str(ip) not in out:
        out.append(str(ip))
if u.hostname:
    add(u.hostname)
    if not out:
        try:
            for info in socket.getaddrinfo(u.hostname, None, socket.AF_INET, socket.SOCK_STREAM):
                add(info[4][0])
        except OSError:
            pass
try:
    r = subprocess.run(["ip", "-4", "route", "get", "1.1.1.1"], capture_output=True, text=True, timeout=2)
    toks = r.stdout.split()
    if "src" in toks:
        add(toks[toks.index("src") + 1], require_global=True)
except Exception:
    pass
print(json.dumps(out))
' "$vm_api_url")
  else
    https_port=$(jq -r --argjson d "$HTTPS_PORT" '.hostHttpsPort // $d' <<<"$merged")
    vm_api_url=https://$HTTPS_HOST:$https_port
  fi
  [[ "$https_port" =~ ^[0-9]+$ ]] && [ "$https_port" -ge 1 ] && [ "$https_port" -le 65535 ] ||
    { echo "stack: invalid hostHttpsPort: $https_port" >&2; exit 2; }
  local untrusted state_size=100G jail_gid
  untrusted=$(jq -r '.untrustedTenants // false' <<<"$merged")
  if [ "$untrusted" = true ]; then
    log "untrustedTenants host settings (module.nix kernel params, as far as a runner allows)"
    # module.nix boots with nosmt=force; boatd refuses to start while SMT control reads "on".
    local smt=/sys/devices/system/cpu/smt/control
    if [ "$(cat "$smt" 2>/dev/null)" = on ]; then echo off | sudo tee "$smt" >/dev/null || true; fi
    echo "SMT control: $(cat "$smt" 2>/dev/null || echo absent), CPUs online: $(nproc)" | tee "$WORK/smt.txt" >&2
    jail_gid=$(jq -r '.jailGid // empty' <<<"$merged")
    if [ -z "$jail_gid" ]; then
      if ! getent group boat-fc >/dev/null 2>&1; then
        sudo groupadd -r -g 899999 boat-fc 2>/dev/null || sudo groupadd -r boat-fc 2>/dev/null || true
      fi
      jail_gid=$(getent group boat-fc 2>/dev/null | cut -d: -f3 || true)
      if ! [[ "$jail_gid" =~ ^[0-9]+$ ]]; then
        jail_gid=$(getent group nogroup 2>/dev/null | cut -d: -f3 || true)
      fi
      [[ "$jail_gid" =~ ^[0-9]+$ ]] || jail_gid=899999
      merged=$(jq -c --argjson gid "$jail_gid" '. + {"jailGid": $gid}' <<<"$merged")
    fi
    sudo sysctl -qw kernel.io_uring_disabled=1 || echo "stack: kernel.io_uring_disabled not settable" >&2
    sudo sysctl -qw "kernel.io_uring_group=$jail_gid" || echo "stack: kernel.io_uring_group not settable" >&2
    [ ! -w /sys/kernel/mm/ksm/run ] || echo 0 | sudo tee /sys/kernel/mm/ksm/run >/dev/null
    # storageOvercommit 1: committed storage (51 GiB disk + 8 GiB per VM) must fit the store.
    state_size=1T
  fi

  log "state: btrfs loop (reflinks like production, $state_size sparse) + slim image"
  [ -f "$WORK/state.img" ] || { truncate -s "$state_size" "$WORK/state.img"; mkfs.btrfs -q "$WORK/state.img"; }
  sudo mkdir -p "$STATE"
  mountpoint -q "$STATE" || sudo mount -o loop "$WORK/state.img" "$STATE"
  local loop_dev
  loop_dev=$(findmnt -no SOURCE "$STATE" 2>/dev/null || true)
  [ -n "$loop_dev" ] || loop_dev=$(losetup -j "$WORK/state.img" 2>/dev/null | cut -d: -f1 || true)
  if [[ "$loop_dev" == /dev/loop* ]]; then
    sudo losetup --direct-io=on "$loop_dev" || true
    local dio
    dio=$(cat "/sys/block/$(basename "$loop_dev")/loop/dio" 2>/dev/null || true)
    if [ "$dio" != "1" ]; then
      echo "WARN: loop direct-io is disabled on $loop_dev (dio=${dio:-0}); storage isolation from host page cache is degraded" >&2
    fi
  fi
  sudo mkdir -p "$STATE/images/slim"
  sudo cp --sparse=always "$guest/vmlinux" "$STATE/images/slim/vmlinux"
  sudo cp --sparse=always "$guest/rootfs-slim.ext4" "$STATE/images/slim/rootfs.ext4"
  if [ "$use_local_tls" = true ]; then
    local mnt=$WORK/guest-ca-mount
    mkdir -p "$mnt"
    mountpoint -q "$mnt" && sudo umount -l "$mnt"
    sudo mount -o loop "$STATE/images/slim/rootfs.ext4" "$mnt"
    seed_guest_ca() {
      sudo mkdir -p "$mnt/etc/ssl/certs"
      { printf '\n'; cat "$WORK/tls/ca.pem"; } | sudo tee -a "$mnt/etc/ssl/certs/ca-certificates.crt" >/dev/null
      sudo cp "$WORK/tls/ca.pem" "$mnt/etc/ssl/certs/boatbench-ca.pem"
      sudo chmod 0644 "$mnt/etc/ssl/certs/ca-certificates.crt" "$mnt/etc/ssl/certs/boatbench-ca.pem"
      sudo grep -qxF "$bridge_ip $HTTPS_HOST" "$mnt/etc/hosts" 2>/dev/null ||
        printf '\n%s %s\n' "$bridge_ip" "$HTTPS_HOST" | sudo tee -a "$mnt/etc/hosts" >/dev/null
      sudo chmod 0644 "$mnt/etc/hosts"
    }
    if ! seed_guest_ca; then
      sudo umount "$mnt" || sudo umount -l "$mnt"
      exit 1
    fi
    sudo umount "$mnt"
    rmdir "$mnt" 2>/dev/null || true
  fi
  sudo rm -rf "$STATE/warm"
  sudo sync
  sudo mkdir -p /sys/fs/cgroup/boat-bench

  # cpuOvercommit: room for BENCH_VMS (12) x 4 vCPU on the CPUs online NOW, i.e. after SMT
  # went off (untrusted leaves a 4-vCPU runner 2 CPUs: 12 x 2 = 24 vCPUs refused workload c's 10 VMs).
  local cpus cpu_oc mem_oc
  cpus=$(nproc)
  cpu_oc=${BENCH_CPU_OVERCOMMIT:-$(( (${BENCH_VMS:-12} * 4 + cpus - 1) / cpus ))}
  [ "${BENCH_CPU_OVERCOMMIT:-}" ] || [ "$cpu_oc" -ge 12 ] || cpu_oc=12
  mem_oc=${BENCH_MEM_OVERCOMMIT:-8}
  if [ "$untrusted" = true ]; then
    if [ -n "${BENCH_MEM_OVERCOMMIT:-}" ] && jq -en --argjson m "$BENCH_MEM_OVERCOMMIT" '($m | type == "number") and $m > 0 and $m <= 1' >/dev/null 2>&1; then
      mem_oc=$BENCH_MEM_OVERCOMMIT
      merged=$(jq -c --argjson m "$mem_oc" '. + {"memOvercommit": $m}' <<<"$merged")
    else
      [ -z "${BENCH_MEM_OVERCOMMIT:-}" ] || echo "stack: clamping invalid untrusted BENCH_MEM_OVERCOMMIT=$BENCH_MEM_OVERCOMMIT to 1 (boatd requires <= 1)" >&2
      mem_oc=1
    fi
  fi
  echo "cpuOvercommit $cpu_oc on $cpus online CPUs (memOvercommit $mem_oc)" | tee -a "$WORK/smt.txt" >&2
  log "boatd (production defaults; memOvercommit=$mem_oc, cpuOvercommit=$cpu_oc): $merged"
  jq --argjson options "$merged" --arg bridge_ip "$bridge_ip" --argjson public_addrs "$public_addrs" --argjson https_port "$https_port" '
    (. * $options)
    | (if .untrustedTenants == true and ((.memOvercommit // 1) > 1 or (.memOvercommit // 1) <= 0) then .memOvercommit = 1 else . end)
    | .hostHttpsPort = (.hostHttpsPort // $https_port)
    | .hostHttpsAddrs = (((.hostHttpsAddrs // []) + $public_addrs + [$bridge_ip]) | unique)
  ' >"$WORK/boatd.json" <<JSON
{"stateDir":"$STATE","runDir":"$WORK/run","socket":"$SOCK","socketGroup":"$(id -gn)",
 "firecracker":"$WORK/dl/firecracker","jailer":"$WORK/dl/jailer","cgroupParent":"boat-bench","reflink":"always",
 "bootTimeoutS":120,"defaultImage":"slim","memOvercommit":$mem_oc,"cpuOvercommit":$cpu_oc,
 "hostHttpsPort":$https_port,"hostHttpsAddrs":["$bridge_ip"],"defaultTeam":{"maxVMs":16}}
JSON
  # Measure the state disk's blk-iocost model once (as boat-iocost-model.service does) so
  # proportional ioCost weight sharing is active alongside untrustedTenants' fail-closed io.max ceiling.
  if jq -e '.untrustedTenants == true or (.ioCost == true and (.ioCostModel // "") == "")' "$WORK/boatd.json" >/dev/null &&
    ! sudo test -s "$STATE/iocost-model"; then
    log "iocost model of the state disk (boatd -measure-disk, about 25 s)"
    sudo "$WORK/bin/boatd" -measure-disk -config "$WORK/boatd.json" | tee "$WORK/iocost-model.txt"
  fi
  sudo rm -f "$SOCK"   # a stale socket from a killed boatd would pass the wait below at once
  sudo bash -c 'echo $$ >"$1"; exec "$2" -config "$3" >"$4" 2>&1' _ \
    "$WORK/boatd.pid" "$WORK/bin/boatd" "$WORK/boatd.json" "$WORK/boatd.log" &
  for _ in $(seq 1200); do [ -S "$SOCK" ] && break; sleep 0.1; done
  curl -fsS --unix-socket "$SOCK" http://boatd/v1/health >/dev/null || { tail -50 "$WORK/boatd.log"; exit 1; }
  # A create before the template exists boots cold and would be reported as warm.
  if [ "$(jq -r .warmPool "$WORK/boatd.json")" = true ]; then
    log "warm template of slim@default"
    for _ in $(seq 300); do sudo test -f "$STATE/warm/slim@default/warm.json" && break; sleep 1; done
    sudo test -f "$STATE/warm/slim@default/warm.json" || { tail -50 "$WORK/boatd.log"; exit 1; }
  fi

  log "boat-api + guest HTTPS control listener ($vm_api_url)"
  stop_pid "$WORK/tls.pid"
  stop_pid "$WORK/api.pid"
  local key
  key=$("$WORK/bin/boat-api" admin init -data "$WORK/api" -user bench -team bench | jq -r .token)
  [[ $key == boat_* ]] || { echo "stack: admin init: $key" >&2; exit 1; }
  "$WORK/bin/boat-api" serve -data "$WORK/api" -listen 127.0.0.1:8740 -boatd "$SOCK" -rate 0 -images slim \
    -ssh-user dev -ssh-policy-helper-file "$ROOT/components/boat/guest/ssh-authorize.py" -vm-api-url "$vm_api_url" "${api_public_args[@]}" >"$WORK/api.log" 2>&1 &
  echo $! >"$WORK/api.pid"
  for _ in $(seq 100); do curl -fs "$API/healthz" >/dev/null && break; sleep 0.1; done
  curl -fs "$API/healthz" >/dev/null || { tail -50 "$WORK/api.log"; exit 1; }
  if [ "$use_local_tls" = true ]; then
    "$WORK/bin/boatbench-tls" -dir "$WORK/tls" -listen "$bridge_ip:$https_port" -upstream "$API" >"$WORK/tls.log" 2>&1 &
    echo $! >"$WORK/tls.pid"
    for _ in $(seq 100); do
      curl -fsS --cacert "$WORK/tls/ca.pem" --resolve "$HTTPS_HOST:$https_port:$bridge_ip" "$vm_api_url/healthz" >/dev/null 2>&1 && break
      sleep 0.1
    done
    curl -fsS --cacert "$WORK/tls/ca.pem" --resolve "$HTTPS_HOST:$https_port:$bridge_ip" "$vm_api_url/healthz" >/dev/null ||
      { tail -50 "$WORK/tls.log"; exit 1; }
    [ "$(curl -s -o /dev/null -w '%{http_code}' --cacert "$WORK/tls/ca.pem" --resolve "$HTTPS_HOST:$https_port:$bridge_ip" "$vm_api_url/internal/ssh/authorize")" = 404 ] ||
      { echo "stack: guest HTTPS proxy exposed /internal" >&2; exit 1; }
  fi
  printf 'export BOAT_URL=%s\nexport BOAT_KEY=%s\nexport BOATD_CONFIG=%q\nexport BOATBENCH_BOATD_OPTIONS=%q\n' "$API" "$key" "$WORK/boatd.json" "$(jq -c 'del(.stateDir, .runDir, .socket, .socketGroup, .firecracker, .jailer)' "$WORK/boatd.json")" >"$WORK/env"
  chmod 600 "$WORK/env"
  log "stack up: $WORK/env"
}

case "${1:-}" in
  up) shift; up "$@" ;;
  down) down ;;
  write-tls-proxy) shift; write_tls_proxy "${1:?usage: stack.sh write-tls-proxy DIR}" ;;
  *) echo "usage: stack.sh up GUEST_DIR | down" >&2; exit 2 ;;
esac
