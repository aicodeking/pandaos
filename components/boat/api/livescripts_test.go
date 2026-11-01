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
