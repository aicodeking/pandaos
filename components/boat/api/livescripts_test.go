package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestLiveScripts runs api/e2e-auth.sh and sdk/e2e-live.sh against this
// package's stack (boat-api + fake boatd), so CI catches script rot before
// boat-e2e.yml runs them against real VMs. Skipped when tools are missing.
func TestLiveScripts(t *testing.T) {
	for _, tool := range []string{"bash", "curl", "jq", "python3", "node", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	if _, err := os.Stat("../sdk/e2e-live.sh"); err != nil {
		t.Skip("not in the source tree (nix builds only this directory)")
	}
	// The real CLI against the real API: e2e-live.sh uses it when it is on PATH.
	bin := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(bin, "boat"), ".")
	build.Dir = "../cli"
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build boat: %v\n%s", err, out)
	}
	for _, script := range []string{"./e2e-auth.sh", "../sdk/e2e-live.sh", "../sdk/e2e-agent-from-env.sh"} {
		t.Run(script, func(t *testing.T) {
			e := setup(t)
			e.srv.cfg.VMDomain = "vm.boat.example"
			cmd := exec.Command("bash", script)
			cmd.Env = append(os.Environ(), "BOAT_URL="+e.api.URL, "BOAT_KEY="+e.owner, "BOAT_API_KEY=", "BOAT_LIVE_FAKE=1",
				"BOAT_CONFIG="+filepath.Join(bin, "cfg.json"), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"ANTHROPIC_API_KEY=", "OPENAI_API_KEY=")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s: %v\n%s", script, err, out)
			}
			if strings.Contains(string(out), "FAIL") {
				t.Fatalf("%s reported a failure:\n%s", script, out)
			}
			t.Logf("%s", out)
			if strings.Contains(script, "agent") && !strings.Contains(string(out), "SKIPPED:") {
				t.Fatalf("with no keys the agent e2e must say it skipped:\n%s", out)
			}
			if n := len(e.boatd.vms); n != 0 {
				t.Errorf("%s left %d VMs behind", script, n)
			}
		})
	}
}

// TestOfficialSDKDrivers runs sdk/compat's official-SDK drivers against this
// package's stack when the SDKs are at hand: BOAT_SDK_PYTHON names a python
// that imports boat_sdk, BOAT_SDK_NODE_PATH a node_modules holding
// @boatdev/sdk. (e2e-live.sh installs both, pinned, and runs the same drivers.)
func TestOfficialSDKDrivers(t *testing.T) {
	for _, c := range []struct{ env, bin, script string }{
		{"BOAT_SDK_PYTHON", "", "../sdk/compat/official_sdk.py"},
		{"BOAT_SDK_NODE_PATH", "node", "../sdk/compat/official_sdk.cjs"},
	} {
		t.Run(c.script, func(t *testing.T) {
			v := os.Getenv(c.env)
			if v == "" {
				t.Skipf("%s not set", c.env)
			}
			e := setup(t)
			bin, extra := c.bin, []string{}
			if bin == "" {
				bin = v
			} else {
				extra = append(extra, "NODE_PATH="+v)
			}
			cmd := exec.Command(bin, c.script)
			cmd.Env = append(append(os.Environ(), "BOAT_URL="+e.api.URL, "BOAT_API_KEY="+e.owner, "BOAT_LIVE_FAKE=1"), extra...)
			out, err := cmd.CombinedOutput()
			if err != nil || strings.Contains(string(out), "FAIL") {
				t.Fatalf("%s: %v\n%s", c.script, err, out)
			}
			t.Logf("%s", out)
			if n := len(e.boatd.vms); n != 0 {
				t.Errorf("%s left %d VMs behind", c.script, n)
			}
		})
	}
}
