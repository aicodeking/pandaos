package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestLiveScripts runs api/e2e-auth.sh and sdk/e2e-live.sh against this
// package's stack (boat-api + fake boatd), so CI catches script rot before
// boat-e2e.yml runs them against real VMs. Skipped when tools are missing.
func TestLiveScripts(t *testing.T) {
	for _, tool := range []string{"bash", "curl", "jq", "python3", "node"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	for _, script := range []string{"./e2e-auth.sh", "../sdk/e2e-live.sh"} {
		t.Run(script, func(t *testing.T) {
			e := setup(t)
			e.srv.cfg.VMDomain = "vm.boat.example"
			cmd := exec.Command("bash", script)
			cmd.Env = append(os.Environ(), "BOAT_URL="+e.api.URL, "BOAT_KEY="+e.owner, "BOAT_API_KEY=")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s: %v\n%s", script, err, out)
			}
			if strings.Contains(string(out), "FAIL") {
				t.Fatalf("%s reported a failure:\n%s", script, out)
			}
			t.Logf("%s", out)
			if n := len(e.boatd.vms); n != 0 {
				t.Errorf("%s left %d VMs behind", script, n)
			}
		})
	}
}
