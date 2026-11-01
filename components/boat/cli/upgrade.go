package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Release layout (shared with cli/install.sh): every pandaos GitHub release
// carries boat_<goos>_<goarch> binaries and boat_SHA256SUMS
// ("<sha256>  <asset>" lines). BOAT_RELEASE_BASE overrides the download
// base (mirrors, tests): <base>/<asset>.
const defaultRepo = "aicodeking/pandaos"

func releaseBase(getenv func(string) string, ver string) string {
	if b := getenv("BOAT_RELEASE_BASE"); b != "" {
		return strings.TrimRight(b, "/")
	}
	repo := getenv("BOAT_REPO")
	if repo == "" {
		repo = defaultRepo
	}
	if ver == "" || ver == "latest" {
		return "https://github.com/" + repo + "/releases/latest/download"
	}
	return "https://github.com/" + repo + "/releases/download/" + ver
}

func assetName() string { return "boat_" + runtime.GOOS + "_" + runtime.GOARCH }

func fetch(hc *http.Client, u string, max int64) ([]byte, error) {
	resp, err := hc.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: HTTP %d", u, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err == nil && int64(len(b)) > max {
		err = fmt.Errorf("GET %s: larger than %d bytes", u, max)
	}
	return b, err
}

// sumFor finds asset's checksum in a SHA256SUMS file.
func sumFor(sums []byte, asset string) (string, bool) {
	sc := bufio.NewScanner(strings.NewReader(string(sums)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == asset && len(f[0]) == 64 {
			return strings.ToLower(f[0]), true
		}
	}
	return "", false
}

// upgrade downloads the release binary, verifies its SHA-256 against
// boat_SHA256SUMS, and atomically replaces the running executable.
func (a *app) upgrade(args []string) error {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	ver := fs.String("version", "latest", "release tag, e.g. v0.3.0")
	if _, _, err := parse(fs, args); err != nil {
		return err
	}
	if a.exe == nil {
		a.exe = os.Executable
	}
	self, err := a.exe()
	if err != nil {
		return err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return err
	}
	base, asset := releaseBase(a.getenv, *ver), assetName()
	hc := &http.Client{Timeout: 5 * time.Minute}
	sums, err := fetch(hc, base+"/boat_SHA256SUMS", 1<<20)
	if err != nil {
		return err
	}
	want, ok := sumFor(sums, asset)
	if !ok {
		return fmt.Errorf("no %s in %s/boat_SHA256SUMS (unsupported platform?)", asset, base)
	}
	bin, err := fetch(hc, base+"/"+asset, 256<<20)
	if err != nil {
		return err
	}
	got := sha256.Sum256(bin)
	if hex.EncodeToString(got[:]) != want {
		return errors.New("checksum mismatch for " + asset + "; not installing")
	}
	tmp, err := os.CreateTemp(filepath.Dir(self), ".boat-upgrade-*")
	if err != nil {
		return fmt.Errorf("cannot write next to %s (reinstall with cli/install.sh or sudo): %w", self, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), self); err != nil {
		return err
	}
	return a.emit(obj{"ok": true, "path": self, "from": version, "to": *ver, "sha256": want}, func() {
		fmt.Fprintf(a.out, "upgraded %s (%s -> %s)\n", self, version, *ver)
	})
}
