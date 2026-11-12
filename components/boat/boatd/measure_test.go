package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMeasureDiskModel(t *testing.T) {
	d := defaultMeasure()
	d.size, d.runFor, d.seqBS = 8<<20, 50*time.Millisecond, 256<<10
	// Buffered: the test dir may be tmpfs (nix build sandbox), which refuses O_DIRECT.
	d.openDir = func(p string) (*os.File, error) { return os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o600) }
	dir := t.TempDir()
	m, err := d.run(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reIOCostModel.MatchString(m) || strings.Contains(m, "=0 ") {
		t.Fatalf("model %q", m)
	}
	if _, err := os.Stat(filepath.Join(dir, probeFile)); err == nil {
		t.Fatal("probe file left behind")
	}
}

// Untrusted with ioCost and no model: refused, naming the command that makes one;
// with stateDir/iocost-model (what boatd -measure-disk writes) it starts.
func TestUntrustedIOCostModelFromStateDir(t *testing.T) {
	dir := t.TempDir()
	c := untrustedConfig(t)
	c.StateDir, c.IOCostModel = dir, ""
	b, _ := json.Marshal(c)
	path := filepath.Join(dir, "boatd.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "boatd -measure-disk -config "+path) || !strings.Contains(err.Error(), "services.boat.ioCostModel") {
		t.Fatalf("missing model: %v", err)
	}
	model := "rbps=3000000000 rseqiops=400000 rrandiops=400000 wbps=2000000000 wseqiops=300000 wrandiops=300000"
	if err := os.WriteFile(filepath.Join(dir, ioCostModelFile), []byte(model+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadConfig(path)
	if err != nil || got.IOCostModel != model {
		t.Fatalf("stored model not used: %q %v", got.IOCostModel, err)
	}
	// The config's own model wins over the stored one.
	c.IOCostModel = "rbps=1 rrandiops=2"
	b, _ = json.Marshal(c)
	os.WriteFile(path, b, 0o600)
	if got, err := loadConfig(path); err != nil || got.IOCostModel != "rbps=1 rrandiops=2" {
		t.Fatalf("config model: %q %v", got.IOCostModel, err)
	}
	// A malformed file is ignored, so the refusal comes back.
	c.IOCostModel = ""
	b, _ = json.Marshal(c)
	os.WriteFile(path, b, 0o600)
	os.WriteFile(filepath.Join(dir, ioCostModelFile), []byte("rbps=x; rm -rf /\n"), 0o644)
	if _, err := loadConfig(path); err == nil {
		t.Fatal("malformed stored model accepted")
	}
}
