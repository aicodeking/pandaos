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

// Untrusted with ioCost and no model (boat-iocost-model has not run, or failed): boatd
// still starts, iocost off and the io.max ceilings on; with stateDir/iocost-model
// (what boatd -measure-disk writes) it uses that model.
func TestUntrustedIOCostModelFromStateDir(t *testing.T) {
	dir := t.TempDir()
	c := untrustedConfig(t)
	c.StateDir, c.IOCostModel = dir, ""
	b, _ := json.Marshal(c)
	path := filepath.Join(dir, "boatd.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadConfig(path)
	if err != nil || got.IOCostModel != "" || got.IOMaxMBps == 0 || got.IOMaxIOPS == 0 {
		t.Fatalf("missing model: want a start on io.max ceilings, got %q %d %d %v", got.IOCostModel, got.IOMaxMBps, got.IOMaxIOPS, err)
	}
	model := "rbps=3000000000 rseqiops=400000 rrandiops=400000 wbps=2000000000 wseqiops=300000 wrandiops=300000"
	if err := os.WriteFile(filepath.Join(dir, ioCostModelFile), []byte(model+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = loadConfig(path)
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
	// A malformed file is ignored: io.max ceilings only.
	c.IOCostModel = ""
	b, _ = json.Marshal(c)
	os.WriteFile(path, b, 0o600)
	os.WriteFile(filepath.Join(dir, ioCostModelFile), []byte("rbps=x; rm -rf /\n"), 0o644)
	if got, err := loadConfig(path); err != nil || got.IOCostModel != "" {
		t.Fatalf("malformed stored model: %q %v", got.IOCostModel, err)
	}
}

// A disk too slow (or too busy) to write the whole probe in the sequential-write
// phase must not fail the read phases at EOF: boat-e2e 43295162352 attempt 1 ran
// -measure-disk right behind an 8 GiB memory dump's writeback.
func TestMeasureDiskSlowWrite(t *testing.T) {
	d := defaultMeasure()
	d.size, d.runFor, d.seqBS = 64<<20, time.Millisecond, 1<<20
	d.openDir = func(p string) (*os.File, error) { return os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o600) }
	if m, err := d.run(t.TempDir()); err != nil || !reIOCostModel.MatchString(m) {
		t.Fatalf("model %q, %v", m, err)
	}
}
