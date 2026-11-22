import base64
import contextlib
import http.client
import http.server
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import signal
import socket
import ssl
import struct
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest import mock
import urllib.parse

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent
STACK_SH = HERE / "stack.sh"
HELPER_PY = ROOT / "components" / "boat" / "guest" / "ssh-authorize.py"


def load_ssh_helper():
    spec = importlib.util.spec_from_file_location("boat_ssh_authorize", HELPER_PY)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


HELPER = load_ssh_helper()
ED25519_BLOB = base64.b64encode(
    struct.pack(">I", len(b"ssh-ed25519")) + b"ssh-ed25519" + struct.pack(">I", 32) + (b"\x11" * 32)
).decode("ascii")


def make_large_rsa_blob():
    key_type = b"ssh-rsa"
    exponent = b"\x01\x00\x01"
    modulus = b"\x00" + (b"\xab" * 2048)
    raw = (
        struct.pack(">I", len(key_type))
        + key_type
        + struct.pack(">I", len(exponent))
        + exponent
        + struct.pack(">I", len(modulus))
        + modulus
    )
    return base64.b64encode(raw).decode("ascii")


LARGE_RSA_BLOB = make_large_rsa_blob()


class StackConfigTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.stack_text = STACK_SH.read_text(encoding="utf-8")

    def test_bash_syntax_and_shellcheck(self):
        proc = subprocess.run(
            ["bash", "-n", str(STACK_SH)],
            capture_output=True,
            text=True,
            timeout=15,
        )
        self.assertEqual(proc.returncode, 0, proc.stderr)
        shellcheck = shutil.which("shellcheck")
        if shellcheck:
            sc = subprocess.run(
                [shellcheck, "-S", "warning", str(STACK_SH)],
                capture_output=True,
                text=True,
                timeout=30,
            )
            self.assertEqual(sc.returncode, 0, sc.stdout + sc.stderr)

    def test_boat_api_ssh_policy_flags_and_contracts(self):
        self.assertTrue(HELPER_PY.is_file(), f"missing SSH helper: {HELPER_PY}")
        helper_size = HELPER_PY.stat().st_size
        self.assertGreater(helper_size, 0)
        self.assertLessEqual(helper_size, 512 * 1024)

        serve_match = re.search(
            r'"\$WORK/bin/boat-api"\s+serve\b(?P<args>(?:\\\n|[^\n])*)',
            self.stack_text,
        )
        self.assertIsNotNone(serve_match, "boat-api serve invocation not found in stack.sh")
        serve_args = re.sub(r"\\\n\s*", " ", serve_match.group("args"))

        self.assertRegex(serve_args, r"(?:^|\s)-ssh-user\s+dev(?:\s|$)")
        self.assertIn('-ssh-policy-helper-file "$ROOT/components/boat/guest/ssh-authorize.py"', serve_args)
        self.assertIn('-vm-api-url "$vm_api_url"', serve_args)

        host_match = re.search(r"^HTTPS_HOST=\$\{BENCH_HTTPS_HOST:-([^}]+)\}", self.stack_text, re.M)
        port_match = re.search(r"^HTTPS_PORT=\$\{BENCH_HTTPS_PORT:-([0-9]+)\}", self.stack_text, re.M)
        self.assertIsNotNone(host_match)
        self.assertIsNotNone(port_match)
        default_host = host_match.group(1)
        default_port = int(port_match.group(1))
        self.assertGreaterEqual(default_port, 1)
        self.assertLessEqual(default_port, 65535)
        self.assertFalse(20000 <= default_port <= 40000, "hostHttpsPort must stay outside boatd portMin..portMax")

        self.assertIn("vm_api_url=https://$HTTPS_HOST:$https_port", self.stack_text)
        self.assertIn('"hostHttpsPort":$https_port', self.stack_text)
        self.assertIn('"hostHttpsAddrs":["$bridge_ip"]', self.stack_text)

        self.assertNotIn("kernel.io_uring_disabled=2", self.stack_text)
        self.assertIn("kernel.io_uring_disabled=1", self.stack_text)
        self.assertIn('kernel.io_uring_group=$jail_gid', self.stack_text)
        self.assertIn("groupadd -r -g 899999 boat-fc", self.stack_text)
        self.assertIn('{"jailGid": $gid}', self.stack_text)

        vm_api_url = f"https://{default_host}:{default_port}"
        parsed = urllib.parse.urlsplit(vm_api_url)
        self.assertEqual(parsed.scheme, "https")
        self.assertTrue(parsed.hostname)
        self.assertIsNone(parsed.username)
        self.assertIsNone(parsed.password)
        self.assertEqual(parsed.query, "")
        self.assertEqual(parsed.fragment, "")
        self.assertIn(parsed.path, ("", "/"))

        authorize_url = vm_api_url.rstrip("/") + "/v1/vms/vm-test_1/ssh/authorize"
        cfg = {
            "version": 1,
            "url": authorize_url,
            "token": "vmtok_test_secret_token",
            "vm_id": "vm-test_1",
            "team_id": "team-1",
            "generation": 1,
            "boot_epoch": "epoch-1",
            "users": ["dev"],
        }
        HELPER.validate_config(cfg)

    def test_guest_ca_seeded_only_into_disposable_copy(self):
        self.assertIn('sudo mount -o loop "$STATE/images/slim/rootfs.ext4" "$mnt"', self.stack_text)
        self.assertNotIn('mount -o loop "$guest/', self.stack_text)
        for line in self.stack_text.splitlines():
            stripped = line.strip()
            if "$guest" in stripped and not stripped.startswith("#"):
                self.assertTrue(
                    stripped.startswith("guest=")
                    or stripped.startswith('sudo cp --sparse=always "$guest/'),
                    f"unexpected access to source guest dir: {stripped}",
                )

    def _make_stub_tools(self, bin_dir: Path, log_file: Path, mounted_file: Path):
        bin_dir.mkdir(parents=True, exist_ok=True)

        def write_exe(name: str, body: str):
            path = bin_dir / name
            path.write_text("#!/usr/bin/env bash\nset -eu\n" + body + "\n", encoding="utf-8")
            path.chmod(0o755)

        write_exe(
            "sudo",
            f'printf "sudo %s\\n" "$*" >>"{log_file}"\nexec "$@"',
        )
        write_exe(
            "pkill",
            f'printf "pkill %s\\n" "$*" >>"{log_file}"\nexit 0',
        )
        write_exe(
            "iptables",
            'if [ "${1:-}" = "-C" ]; then exit 1; fi\nexit 0',
        )
        write_exe(
            "ip",
            f'printf "ip %s\\n" "$*" >>"{log_file}"\n'
            'if [ "${1:-}" = "-o" ]; then printf "1: lo: <LOOPBACK>\\n2: bt7: <BROADCAST>\\n"; fi\nexit 0',
        )
        write_exe(
            "nft",
            f'printf "nft %s\\n" "$*" >>"{log_file}"\nexit 0',
        )
        write_exe(
            "mountpoint",
            'target="${2:-}"\n'
            f'grep -qxF "$target" "{mounted_file}" 2>/dev/null',
        )
        write_exe(
            "umount",
            'target="${2:-${1:-}}"\n'
            f'printf "umount %s\\n" "$*" >>"{log_file}"\n'
            f'python3 -c \'import pathlib,sys; p=pathlib.Path(sys.argv[1]); t=sys.argv[2]; '
            f'lines=[x for x in p.read_text().splitlines() if x!=t] if p.exists() else []; '
            f'p.write_text("\\n".join(lines)+("\\n" if lines else ""))\' "{mounted_file}" "$target"\n'
            "exit 0",
        )

    def test_down_cleans_up_tls_api_and_boatd_pids_and_mounts(self):
        with tempfile.TemporaryDirectory(prefix="boatbench-stack-down-") as tmp:
            root = Path(tmp)
            work = root / "work"
            state = root / "state"
            work.mkdir()
            state.mkdir()
            (work / "guest-ca-mount").mkdir()

            log_file = root / "calls.log"
            mounted_file = root / "mounted.txt"
            mounted_file.write_text(f"{work / 'guest-ca-mount'}\n{state}\n", encoding="utf-8")
            bin_dir = root / "bin"
            self._make_stub_tools(bin_dir, log_file, mounted_file)

            tls_proc = subprocess.Popen(
                [sys.executable, "-c", "import signal,time; signal.signal(signal.SIGTERM, signal.SIG_IGN); time.sleep(60)"]
            )
            api_proc = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(60)"])
            boatd_proc = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(60)"])
            try:
                (work / "tls.pid").write_text(f"{tls_proc.pid}\n", encoding="utf-8")
                (work / "api.pid").write_text(f"{api_proc.pid}\n", encoding="utf-8")
                (work / "boatd.pid").write_text(f"{boatd_proc.pid}\n", encoding="utf-8")

                env = os.environ.copy()
                env["BENCH_WORK"] = str(work)
                env["BENCH_STATE"] = str(state)
                env["PATH"] = f"{bin_dir}:{env.get('PATH', '')}"

                res = subprocess.run(
                    ["bash", str(STACK_SH), "down"],
                    env=env,
                    capture_output=True,
                    text=True,
                    timeout=20,
                )
                self.assertEqual(res.returncode, 0, res.stderr)

                for name, proc in (("tls", tls_proc), ("api", api_proc), ("boatd", boatd_proc)):
                    rc = proc.wait(timeout=3)
                    self.assertIsNotNone(rc, f"{name} process still alive")
                    self.assertFalse((work / f"{name}.pid").exists(), f"{name}.pid was not removed")

                self.assertEqual(mounted_file.read_text(encoding="utf-8").strip(), "")
                calls = log_file.read_text(encoding="utf-8")
                self.assertIn("ip link del bt7", calls)
                self.assertIn("ip link del boatbr0", calls)
            finally:
                for proc in (tls_proc, api_proc, boatd_proc):
                    if proc.poll() is None:
                        proc.kill()
                        proc.wait(timeout=3)

    def test_down_handles_missing_and_stale_pidfiles(self):
        with tempfile.TemporaryDirectory(prefix="boatbench-stack-stale-") as tmp:
            root = Path(tmp)
            work = root / "work"
            state = root / "state"
            work.mkdir()
            state.mkdir()
            log_file = root / "calls.log"
            mounted_file = root / "mounted.txt"
            mounted_file.write_text("", encoding="utf-8")
            bin_dir = root / "bin"
            self._make_stub_tools(bin_dir, log_file, mounted_file)

            dead = subprocess.Popen([sys.executable, "-c", "pass"])
            dead.wait(timeout=5)
            (work / "tls.pid").write_text(f"{dead.pid}\n", encoding="utf-8")
            (work / "api.pid").write_text("not-a-pid\n", encoding="utf-8")

            env = os.environ.copy()
            env["BENCH_WORK"] = str(work)
            env["BENCH_STATE"] = str(state)
            env["PATH"] = f"{bin_dir}:{env.get('PATH', '')}"

            res = subprocess.run(
                ["bash", str(STACK_SH), "down"],
                env=env,
                capture_output=True,
                text=True,
                timeout=15,
            )
            self.assertEqual(res.returncode, 0, res.stderr)
            self.assertFalse((work / "tls.pid").exists())
            self.assertFalse((work / "api.pid").exists())

    def test_loop_direct_io_enabled_on_state_store(self):
        self.assertIn('mountpoint -q "$STATE" || sudo mount -o loop "$WORK/state.img" "$STATE"', self.stack_text)
        self.assertNotIn('losetup --direct-io=on "$loop_dev" 2>/dev/null', self.stack_text)
        self.assertIn('sudo losetup --direct-io=on "$loop_dev" || true', self.stack_text)
        self.assertIn('/sys/block/$(basename "$loop_dev")/loop/dio', self.stack_text)
        self.assertIn(
            'echo "WARN: loop direct-io is disabled on $loop_dev (dio=${dio:-0}); '
            'storage isolation from host page cache is degraded" >&2',
            self.stack_text,
        )

    def _extract_stack_jq_and_base_cfg(self, *, https_port=18443, bridge_ip="10.200.0.1", mem_oc=8, cpu_oc=12):
        jq_match = re.search(
            r"jq\s+--argjson\s+options\s+\"\$merged\"[^\n]*'\n(?P<expr>.*?)\n\s*'\s*>\"\$WORK/boatd\.json\"\s*<<JSON\n(?P<tmpl>.*?)\nJSON",
            self.stack_text,
            re.S,
        )
        self.assertIsNotNone(jq_match, "boatd.json jq merge expression and base JSON heredoc not found in stack.sh")
        jq_expr = jq_match.group("expr")
        tmpl = jq_match.group("tmpl")
        rendered = (
            tmpl.replace("$STATE", "/tmp/boatbench-state")
            .replace("$WORK", "/tmp/boatbench-work")
            .replace("$SOCK", "/tmp/boatbench-work/run/boatd.sock")
            .replace("$(id -gn)", "staff")
            .replace("$mem_oc", str(mem_oc))
            .replace("$cpu_oc", str(cpu_oc))
            .replace("$https_port", str(https_port))
            .replace("$bridge_ip", bridge_ip)
        )
        self.assertNotIn("$", rendered, f"unresolved shell variable in stack.sh base JSON template: {rendered}")
        return jq_expr, json.loads(rendered)

    def _run_stack_jq_merge(self, jq_expr, base_cfg, options, *, bridge_ip="10.200.0.1", public_addrs=(), https_port=18443):
        proc = subprocess.run(
            [
                "jq",
                "-c",
                "--argjson", "options", json.dumps(options),
                "--arg", "bridge_ip", bridge_ip,
                "--argjson", "public_addrs", json.dumps(list(public_addrs)),
                "--argjson", "https_port", str(https_port),
                jq_expr,
            ],
            input=json.dumps(base_cfg),
            capture_output=True,
            text=True,
            timeout=10,
        )
        self.assertEqual(proc.returncode, 0, proc.stderr)
        return json.loads(proc.stdout)

    @unittest.skipUnless(shutil.which("jq"), "jq required for boatd.json merge check")
    def test_untrusted_local_tls_vs_public_url_bootstrap_in_stack_sh(self):
        presets = json.loads((HERE / "presets.json").read_text(encoding="utf-8"))
        untrusted = presets["untrusted"]
        self.assertEqual(untrusted.get("memOvercommit"), 1)

        # Extract the exact jq filter and base boatd.json template (with memOvercommit: 8) from stack.sh.
        self.assertIn('if [[ "${PUBLIC_URL:-}" == https://* ]]; then', self.stack_text)
        self.assertIn("use_local_tls=false", self.stack_text)
        self.assertIn('api_public_args=(-public-url "$vm_api_url")', self.stack_text)

        jq_expr, base_cfg = self._extract_stack_jq_and_base_cfg(https_port=18443, mem_oc=8)
        self.assertEqual(base_cfg["memOvercommit"], 8)

        # Local untrusted run (PUBLIC_URL unset or http://...): enforces hostHttpsPort + 10.200.0.1 even if overridden empty,
        # and sets memOvercommit <= 1 even when merged onto base memOvercommit: 8.
        merged_override = dict(untrusted, hostHttpsAddrs=[])
        out = self._run_stack_jq_merge(jq_expr, base_cfg, merged_override, https_port=18443)
        self.assertTrue(out["untrustedTenants"])
        self.assertEqual(out["memOvercommit"], 1)
        self.assertEqual(out["hostHttpsPort"], 18443)
        self.assertEqual(out["hostHttpsAddrs"], ["10.200.0.1"])

        # Even if an untrusted option map omits memOvercommit or passes > 1, the jq guard clamps it to 1.
        omitted_mem = {k: v for k, v in untrusted.items() if k != "memOvercommit"}
        self.assertEqual(self._run_stack_jq_merge(jq_expr, base_cfg, omitted_mem)["memOvercommit"], 1)
        raised_mem = dict(untrusted, memOvercommit=8)
        self.assertEqual(self._run_stack_jq_merge(jq_expr, base_cfg, raised_mem)["memOvercommit"], 1)
        valid_sub1 = dict(untrusted, memOvercommit=0.8)
        self.assertAlmostEqual(self._run_stack_jq_merge(jq_expr, base_cfg, valid_sub1)["memOvercommit"], 0.8)

        # PUBLIC_URL=https://... mode with a public IPv4 (via BOATBENCH_BOATD_OPTIONS or resolved PUBLIC_URL):
        # boatd honors hostHttpsAddrs literally, so 10.200.0.1 must remain explicitly listed alongside the public IPv4.
        _, base_pub_cfg = self._extract_stack_jq_and_base_cfg(https_port=443, mem_oc=8)
        for opts, pub_addrs in (
            (dict(untrusted, hostHttpsPort=443, hostHttpsAddrs=["203.0.113.10"]), []),
            (dict(untrusted), ["203.0.113.10"]),
        ):
            out_pub = self._run_stack_jq_merge(jq_expr, base_pub_cfg, opts, public_addrs=pub_addrs, https_port=443)
            self.assertEqual(out_pub["memOvercommit"], 1)
            self.assertEqual(out_pub["hostHttpsPort"], 443)
            self.assertIn("10.200.0.1", out_pub["hostHttpsAddrs"])
            self.assertIn("203.0.113.10", out_pub["hostHttpsAddrs"])
            self.assertEqual(len(out_pub["hostHttpsAddrs"]), 2)

    @unittest.skipUnless(shutil.which("jq"), "jq required for stack.sh BENCH_MEM_OVERCOMMIT check")
    def test_untrusted_bench_mem_overcommit_shell_clamping(self):
        # Extract the exact mem_oc clamping block from stack.sh and execute it under bash across cases.
        block_match = re.search(
            r"(?P<block>mem_oc=\$\{BENCH_MEM_OVERCOMMIT:-8\}\n  if \[ \"\$untrusted\" = true \]; then.*?\n  fi)\n",
            self.stack_text,
            re.S,
        )
        self.assertIsNotNone(block_match, "mem_oc untrusted clamping block not found in stack.sh")
        block = block_match.group("block")
        presets = json.loads((HERE / "presets.json").read_text(encoding="utf-8"))
        for untrusted_val, env_val, expected_oc, expect_warn in (
            ("true", None, 1, False),
            ("true", "0.75", 0.75, False),
            ("true", "1", 1, False),
            ("true", "8", 1, True),
            ("true", "0", 1, True),
            ("true", "bogus", 1, True),
            ("false", None, 8, False),
            ("false", "4", 4, False),
        ):
            env = os.environ.copy()
            if env_val is None:
                env.pop("BENCH_MEM_OVERCOMMIT", None)
            else:
                env["BENCH_MEM_OVERCOMMIT"] = env_val
            merged_in = json.dumps(presets["untrusted"] if untrusted_val == "true" else presets["trusted"])
            script = (
                "set -euo pipefail\n"
                f"untrusted={untrusted_val}\n"
                f"merged='{merged_in}'\n"
                f"{block}\n"
                'printf "%s\\n%s\\n" "$mem_oc" "$merged"\n'
            )
            res = subprocess.run(["bash", "-c", script], env=env, capture_output=True, text=True, timeout=10)
            self.assertEqual(res.returncode, 0, res.stderr)
            lines = res.stdout.strip().splitlines()
            self.assertAlmostEqual(float(lines[0]), expected_oc)
            merged_out = json.loads(lines[1])
            if untrusted_val == "true":
                self.assertAlmostEqual(float(merged_out["memOvercommit"]), expected_oc)
            if expect_warn:
                self.assertIn("clamping invalid untrusted BENCH_MEM_OVERCOMMIT", res.stderr)
            else:
                self.assertEqual(res.stderr.strip(), "")

    @unittest.skipUnless(shutil.which("jq") and shutil.which("go"), "jq and go required for boatd loadConfig check")
    def test_all_presets_validate_against_real_boatd_load_config(self):
        presets = json.loads((HERE / "presets.json").read_text(encoding="utf-8"))
        preset_names = [k for k in presets if not k.startswith("_")]
        self.assertGreaterEqual(len(preset_names), 4)

        jq_expr, base_cfg = self._extract_stack_jq_and_base_cfg(https_port=8443, mem_oc=8)
        boatd_src = ROOT / "components" / "boat" / "boatd"
        self.assertTrue((boatd_src / "config.go").is_file(), f"missing boatd config.go: {boatd_src}")

        with tempfile.TemporaryDirectory(prefix="boatbench-boatd-cfg-") as tmp:
            tmp_dir = Path(tmp)
            pkg_dir = tmp_dir / "boatd"
            cfg_dir = tmp_dir / "cfgs"
            pkg_dir.mkdir()
            cfg_dir.mkdir()

            shutil.copy2(boatd_src / "go.mod", pkg_dir / "go.mod")
            shutil.copy2(boatd_src / "config.go", pkg_dir / "config.go")

            valid_paths = []
            for name in preset_names:
                merged = self._run_stack_jq_merge(jq_expr, base_cfg, presets[name], https_port=8443)
                if name == "untrusted":
                    self.assertTrue(merged["untrustedTenants"])
                    self.assertEqual(merged["memOvercommit"], 1)
                else:
                    self.assertEqual(merged["memOvercommit"], 8)
                p = cfg_dir / f"{name}.json"
                p.write_text(json.dumps(merged, indent=2) + "\n", encoding="utf-8")
                valid_paths.append(p)

            # Also validate untrusted with PUBLIC_URL=https://... + resolved public IPv4.
            _, base_pub_cfg = self._extract_stack_jq_and_base_cfg(https_port=443, mem_oc=8)
            pub_merged = self._run_stack_jq_merge(
                jq_expr, base_pub_cfg, presets["untrusted"], public_addrs=["203.0.113.10"], https_port=443
            )
            pub_path = cfg_dir / "untrusted-public.json"
            pub_path.write_text(json.dumps(pub_merged, indent=2) + "\n", encoding="utf-8")
            valid_paths.append(pub_path)

            # Negative control: untrustedTenants=true with memOvercommit=8 must be rejected by real boatd loadConfig().
            bad_cfg = dict(self._run_stack_jq_merge(jq_expr, base_cfg, presets["untrusted"], https_port=8443))
            bad_cfg["memOvercommit"] = 8
            bad_path = cfg_dir / "bad-untrusted-memovercommit.json"
            bad_path.write_text(json.dumps(bad_cfg, indent=2) + "\n", encoding="utf-8")

            test_go = """package main

import (
\t"os"
\t"path/filepath"
\t"strings"
\t"testing"
\t"time"
)

const (
\tledgerDefaultRetention = 400 * 24 * time.Hour
\tmeasureCmd             = "boatd -measure-disk"
\tioCostModelFile        = "iocost-model"
\twarmTeam               = "_warm"
)

type VM struct{ Team string }

var privateV4 = []string{
\t"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
\t"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
\t"192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
\t"224.0.0.0/4", "240.0.0.0/4",
}

func storedIOCostModel(string) string { return "" }

func TestValidateStackPresets(t *testing.T) {
\tdir := os.Getenv("BOATBENCH_CFG_DIR")
\tif dir == "" {
\t\tt.Fatal("BOATBENCH_CFG_DIR unset")
\t}
\tentries, err := os.ReadDir(dir)
\tif err != nil {
\t\tt.Fatalf("readdir %s: %v", dir, err)
\t}
\tchecked := 0
\tfor _, e := range entries {
\t\tif !strings.HasSuffix(e.Name(), ".json") {
\t\t\tcontinue
\t\t}
\t\tp := filepath.Join(dir, e.Name())
\t\tcfg, err := loadConfig(p)
\t\tif strings.HasPrefix(e.Name(), "bad-") {
\t\t\tif err == nil || !strings.Contains(err.Error(), "memOvercommit") {
\t\t\t\tt.Fatalf("%s: expected memOvercommit rejection, got cfg=%+v err=%v", e.Name(), cfg.MemOvercommit, err)
\t\t\t}
\t\t\tcontinue
\t\t}
\t\tif err != nil {
\t\t\tt.Fatalf("%s: loadConfig rejected merged preset: %v", e.Name(), err)
\t\t}
\t\tif cfg.UntrustedTenants && cfg.MemOvercommit > 1 {
\t\t\tt.Fatalf("%s: untrustedTenants accepted memOvercommit=%v", e.Name(), cfg.MemOvercommit)
\t\t}
\t\tchecked++
\t}
\tif checked < 5 {
\t\tt.Fatalf("checked only %d valid preset configs", checked)
\t}
}
"""
            (pkg_dir / "stack_presets_check_test.go").write_text(test_go, encoding="utf-8")

            go_env = os.environ.copy()
            go_env["GOPROXY"] = "off"
            go_env["CGO_ENABLED"] = "0"
            go_env["BOATBENCH_CFG_DIR"] = str(cfg_dir)
            proc = subprocess.run(
                ["go", "test", "-count=1", "-v", "-run", "^TestValidateStackPresets$", "."],
                cwd=str(pkg_dir),
                env=go_env,
                capture_output=True,
                text=True,
                timeout=60,
            )
            self.assertEqual(proc.returncode, 0, proc.stdout + proc.stderr)

    @unittest.skipUnless(shutil.which("go"), "go toolchain required for TLS proxy integration check")
    def test_tls_proxy_satisfies_guest_ssh_authorize_end_to_end(self):
        with tempfile.TemporaryDirectory(prefix="boatbench-tls-e2e-") as tmp:
            root = Path(tmp)
            tls_dir = root / "tls"
            bin_path = root / "boatbench-tls"

            gen = subprocess.run(
                ["bash", str(STACK_SH), "write-tls-proxy", str(tls_dir)],
                capture_output=True,
                text=True,
                timeout=15,
            )
            self.assertEqual(gen.returncode, 0, gen.stderr)

            build_env = os.environ.copy()
            build_env["CGO_ENABLED"] = "0"
            build = subprocess.run(
                ["go", "build", "-o", str(bin_path), "."],
                cwd=str(tls_dir),
                env=build_env,
                capture_output=True,
                text=True,
                timeout=60,
            )
            self.assertEqual(build.returncode, 0, build.stderr)

            init_res = subprocess.run(
                [str(bin_path), "-init", "-dir", str(tls_dir), "-host", "api.boat.test", "-subnet", "10.200.0.0/16"],
                capture_output=True,
                text=True,
                timeout=15,
            )
            self.assertEqual(init_res.returncode, 0, init_res.stderr)
            self.assertEqual(init_res.stdout.strip(), "10.200.0.1")
            self.assertTrue((tls_dir / "ca.pem").is_file())
            self.assertTrue((tls_dir / "cert.pem").is_file())
            self.assertEqual((tls_dir / "key.pem").stat().st_mode & 0o777, 0o600)

            seen_requests = []

            class MockBoatAPI(http.server.BaseHTTPRequestHandler):
                def log_message(self, *args):
                    pass

                def do_GET(self):
                    seen_requests.append(("GET", self.path, dict(self.headers), b""))
                    if self.path == "/healthz":
                        body = b"ok\n"
                        self.send_response(200)
                        self.send_header("Content-Type", "text/plain; charset=utf-8")
                        self.send_header("Content-Length", str(len(body)))
                        self.end_headers()
                        self.wfile.write(body)
                        return
                    self.send_response(404)
                    self.send_header("Content-Length", "0")
                    self.end_headers()

                def do_POST(self):
                    length = int(self.headers.get("Content-Length", "0"))
                    raw = self.rfile.read(length)
                    seen_requests.append(("POST", self.path, dict(self.headers), raw))
                    if self.path == "/internal/ssh/authorize":
                        body = b"should never be reachable\n"
                        self.send_response(200)
                        self.send_header("Content-Length", str(len(body)))
                        self.end_headers()
                        self.wfile.write(body)
                        return
                    if self.path == "/v1/vms/vm-1/ssh/authorize":
                        req = json.loads(raw.decode("utf-8"))
                        if req["key_blob"] in (ED25519_BLOB, LARGE_RSA_BLOB):
                            payload = (
                                json.dumps({"authorized_key": f"{req['key_type']} {req['key_blob']}"}) + "\n"
                            ).encode("utf-8")
                            self.send_response(200)
                            self.send_header("Content-Type", "application/json")
                            self.send_header("Content-Length", str(len(payload)))
                            self.end_headers()
                            self.wfile.write(payload)
                            return
                        self.send_response(204)
                        self.end_headers()
                        return
                    self.send_response(404)
                    self.send_header("Content-Length", "0")
                    self.end_headers()

            upstream = http.server.ThreadingHTTPServer(("127.0.0.1", 0), MockBoatAPI)
            upstream.daemon_threads = True
            upstream_thread = threading.Thread(target=upstream.serve_forever)
            upstream_thread.start()

            with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as probe_sock:
                probe_sock.bind(("127.0.0.1", 0))
                tls_port = probe_sock.getsockname()[1]

            proxy_proc = subprocess.Popen(
                [
                    str(bin_path),
                    "-dir",
                    str(tls_dir),
                    "-listen",
                    f"127.0.0.1:{tls_port}",
                    "-upstream",
                    f"http://127.0.0.1:{upstream.server_port}",
                ],
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
            )
            try:
                ca_file = str(tls_dir / "ca.pem")
                client_ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
                client_ctx.load_verify_locations(cafile=ca_file)

                ready = False
                for _ in range(50):
                    try:
                        conn = http.client.HTTPSConnection("127.0.0.1", tls_port, timeout=1.0, context=client_ctx)
                        conn.request(
                            "GET",
                            "/healthz",
                            headers={
                                "Host": f"api.boat.test:{tls_port}",
                                "X-Forwarded-For": "10.0.0.99",
                                "X-Forwarded-Proto": "http",
                            },
                        )
                        resp = conn.getresponse()
                        body = resp.read()
                        conn.close()
                        if resp.status == 200 and body == b"ok\n":
                            ready = True
                            break
                    except OSError:
                        time.sleep(0.05)
                self.assertTrue(ready, "boatbench-tls did not become ready")

                _, _, health_headers, _ = seen_requests[-1]
                lowered = {k.lower(): v for k, v in health_headers.items()}
                self.assertEqual(lowered.get("x-forwarded-for"), "127.0.0.1")
                self.assertEqual(lowered.get("x-forwarded-proto"), "https")

                for blocked in (
                    "/internal/ssh/authorize",
                    "/v1/../internal/ssh/authorize",
                    "/v1/%2e%2e/internal/ssh/authorize",
                ):
                    before = len(seen_requests)
                    conn = http.client.HTTPSConnection("127.0.0.1", tls_port, timeout=1.0, context=client_ctx)
                    conn.request("POST", blocked, body=b"{}", headers={"Host": f"api.boat.test:{tls_port}"})
                    resp = conn.getresponse()
                    resp.read()
                    conn.close()
                    self.assertEqual(resp.status, 404, f"expected 404 for {blocked}")
                    self.assertEqual(len(seen_requests), before, f"blocked route {blocked} reached upstream")

                cfg = {
                    "version": 1,
                    "url": f"https://api.boat.test:{tls_port}/v1/vms/vm-1/ssh/authorize",
                    "token": "vmtok_live_test",
                    "vm_id": "vm-1",
                    "team_id": "team-1",
                    "generation": 1,
                    "boot_epoch": "epoch-1",
                    "users": ["dev"],
                }

                orig_load_verify = ssl.SSLContext.load_verify_locations
                orig_getaddrinfo = socket.getaddrinfo

                def load_local_ca(ctx, *args, **kwargs):
                    return orig_load_verify(ctx, cafile=ca_file)

                def resolve_boat_test(host, port, *args, **kwargs):
                    if host == "api.boat.test":
                        return orig_getaddrinfo("127.0.0.1", port, *args, **kwargs)
                    return orig_getaddrinfo(host, port, *args, **kwargs)

                with mock.patch.object(ssl.SSLContext, "load_verify_locations", load_local_ca), mock.patch.object(
                    socket, "getaddrinfo", resolve_boat_test
                ):
                    line_small = HELPER.lookup(cfg, "dev", "ssh-ed25519", ED25519_BLOB)
                    self.assertEqual(line_small, f"ssh-ed25519 {ED25519_BLOB}")

                    self.assertGreater(len(LARGE_RSA_BLOB), 2700)
                    line_large = HELPER.lookup(cfg, "dev", "ssh-rsa", LARGE_RSA_BLOB)
                    self.assertEqual(line_large, f"ssh-rsa {LARGE_RSA_BLOB}")

                    other_ed25519 = base64.b64encode(
                        struct.pack(">I", len(b"ssh-ed25519"))
                        + b"ssh-ed25519"
                        + struct.pack(">I", 32)
                        + (b"\x22" * 32)
                    ).decode("ascii")
                    with self.assertRaisesRegex(ValueError, "authorization denied"):
                        HELPER.lookup(cfg, "dev", "ssh-ed25519", other_ed25519)

                proxy_proc.send_signal(signal.SIGTERM)
                self.assertEqual(proxy_proc.wait(timeout=5), 0)
            finally:
                if proxy_proc.poll() is None:
                    proxy_proc.kill()
                    proxy_proc.wait(timeout=3)
                if proxy_proc.stdout:
                    proxy_proc.stdout.close()
                if proxy_proc.stderr:
                    proxy_proc.stderr.close()
                upstream.shutdown()
                upstream.server_close()
                upstream_thread.join(timeout=3)

    def test_stack_env_exports_boatd_config_and_workflow_captures_desc_after_stack_up(self):
        stack_src = Path(__file__).resolve().parent.joinpath("stack.sh").read_text(encoding="utf-8")
        self.assertIn('export BOATD_CONFIG=%q', stack_src)
        self.assertIn('"$WORK/boatd.json"', stack_src)

        wf_path = Path(__file__).resolve().parents[2] / ".github" / "workflows" / "boatbench.yml"
        wf_src = wf_path.read_text(encoding="utf-8")
        stack_up_idx = wf_src.index("bench/boatbench/stack.sh up")
        host_desc_idx = wf_src.index("BOATBENCH_HOST_DESC=GitHub ubuntu-latest runner:")
        self.assertGreater(
            host_desc_idx, stack_up_idx,
            "BOATBENCH_HOST_DESC must be captured after stack.sh up so untrusted SMT-off CPU count is accurate",
        )


if __name__ == "__main__":
    unittest.main()
