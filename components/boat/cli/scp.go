package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// cmdSCP copies through the files API, like boat.dev's boat scp:
//
//	boat scp ./a.txt vm:a.txt        boat scp vm:/tmp/out.zip ./out.zip
//	boat scp -r ./dir vm:dir         boat scp -r vm:dir ./here
//
// A relative remote path is from the VM user's home; /home/user/… (boat.dev's
// home) means that home too. A remote path ending in / gets the source's name.
func cmdSCP(a *app, args []string) error {
	fs := flag.NewFlagSet("scp", flag.ContinueOnError)
	rec := fs.Bool("r", false, "copy directories recursively")
	fs.BoolVar(rec, "recursive", false, "same as -r")
	pos, _, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 2, "<src> <dst> with vm:path on one side"); err != nil {
		return err
	}
	src, dst := pos[0], pos[1]
	srcVM, srcPath, srcRemote := splitRemote(src)
	dstVM, dstPath, dstRemote := splitRemote(dst)
	if srcRemote == dstRemote {
		return fmt.Errorf("exactly one of src/dst must be vm:path: %w", errUsage)
	}
	if dstRemote {
		id, err := vmArg(a, dstVM)
		if err != nil {
			return err
		}
		fi, err := os.Stat(src)
		if err != nil {
			return err
		}
		if strings.HasSuffix(dstPath, "/") || dstPath == "" {
			dstPath += filepath.Base(filepath.Clean(src))
		}
		if fi.IsDir() {
			if !*rec {
				return fmt.Errorf("%s is a directory (use -r): %w", src, errUsage)
			}
			return a.scpDirUp(id, src, dstVM, homeRel(dstPath))
		}
		return a.scpFileUp(id, src, dstVM, dstPath)
	}
	id, err := vmArg(a, srcVM)
	if err != nil {
		return err
	}
	if *rec {
		return a.scpDirDown(id, homeRel(srcPath), dst)
	}
	return a.scpFileDown(id, src, srcPath, dst)
}

// splitRemote reads vm:path. A one-letter "vm" is a Windows drive, and a
// "vm" with a slash is a local path with a colon.
func splitRemote(p string) (vm, path string, remote bool) {
	vm, path, ok := strings.Cut(p, ":")
	if !ok || len(vm) < 2 || strings.ContainsAny(vm, `/\`) {
		return "", p, false
	}
	return vm, path, true
}

// homeRel maps boat.dev's /home/user to the home-relative form ("" = home).
func homeRel(p string) string {
	if p == "/home/user" || strings.HasPrefix(p, "/home/user/") {
		return strings.TrimPrefix(strings.TrimPrefix(p, "/home/user"), "/")
	}
	return p
}

// absRemote makes a remote path absolute: the files API wants one, and only
// the VM knows its home.
func (a *app) absRemote(id, p string) (string, error) {
	p = homeRel(p)
	if strings.HasPrefix(p, "/") {
		return p, nil
	}
	home, err := a.quiet(id, []string{"sh", "-c", `printf %s "$HOME"`})
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(home, "/") {
		return "", errors.New("the VM reported no home directory")
	}
	return path.Join(home, p), nil
}

func (a *app) scpFileUp(id, src, vmName, dstPath string) error {
	abs, err := a.absRemote(id, dstPath)
	if err != nil {
		return err
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	resp, err := a.c.request("PUT", vmPath(id, "files")+"?path="+url.QueryEscape(abs), f, "application/octet-stream")
	if err != nil {
		return err
	}
	var out obj
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	return a.emit(out, func() { fmt.Fprintf(a.out, "%s -> %s:%s (%v bytes)\n", src, vmName, abs, out["bytes"]) })
}

func (a *app) scpDirUp(id, src, vmName, to string) error {
	abs, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	files, err := walkPushFiles(abs) // scp copies everything, not only what git tracks
	if err != nil {
		return err
	}
	r, err := a.pushFiles(id, abs, files, to)
	if err != nil {
		return err
	}
	return a.okLine(obj{"ok": true, "id": id, "path": r.Path, "files": r.Files},
		fmt.Sprintf("%s -> %s:%s (%d files)", src, vmName, r.Path, r.Files))
}

func (a *app) scpFileDown(id, src, srcPath, dst string) error {
	abs, err := a.absRemote(id, srcPath)
	if err != nil {
		return err
	}
	resp, err := a.c.request("GET", vmPath(id, "files")+"?path="+url.QueryEscape(abs), nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if st, err := os.Stat(dst); err == nil && st.IsDir() {
		dst = filepath.Join(dst, path.Base(abs))
	}
	n, err := writeFile(dst, resp.Body)
	if err != nil {
		return err
	}
	return a.emit(obj{"ok": true, "path": dst, "bytes": n}, func() { fmt.Fprintf(a.out, "%s -> %s (%d bytes)\n", src, dst, n) })
}

func writeFile(dst string, r io.Reader) (int64, error) {
	f, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return n, err
}

// The VM packs the directory into a temp file the files API then serves.
const packScript = `d=$0; case "$d" in /*) ;; *) d=$HOME/$d ;; esac
[ -d "$d" ] || { echo "no such directory: $d" >&2; exit 1; }
tar czf "$1" -C "$(dirname -- "$d")" -- "$(basename -- "$d")"`

// scpDirDown copies the VM's directory src into dst (dst/<base of src> when
// dst is an existing directory, else dst itself).
func (a *app) scpDirDown(id, src, dst string) error {
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmp := "/tmp/.boat-scp-" + hex.EncodeToString(rnd[:]) + ".tgz"
	if _, err := a.quiet(id, []string{"bash", "-c", packScript, src, tmp}); err != nil {
		return fmt.Errorf("pack on the VM: %w", err)
	}
	defer a.quiet(id, []string{"rm", "-f", "--", tmp})
	resp, err := a.c.request("GET", vmPath(id, "files")+"?path="+url.QueryEscape(tmp), nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	target := dst
	if st, err := os.Stat(dst); err == nil && st.IsDir() {
		target = filepath.Join(dst, path.Base(strings.TrimRight(src, "/")))
	}
	n, err := untarInto(resp.Body, target)
	if err != nil {
		return err
	}
	return a.okLine(obj{"ok": true, "path": target, "files": n}, fmt.Sprintf("%s -> %s (%d files)", src, target, n))
}

// untarInto unpacks a .tgz of one top directory into target (renaming the
// top directory). Entries that would land outside target are refused;
// symlinks are skipped.
func untarInto(r io.Reader, target string) (int, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, err
	}
	tr := tar.NewReader(gz)
	n := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		_, rest, _ := strings.Cut(strings.TrimPrefix(path.Clean(h.Name), "./"), "/")
		if !filepath.IsLocal(filepath.FromSlash(orDot(rest))) {
			return n, fmt.Errorf("unsafe path in archive: %s", h.Name)
		}
		p := filepath.Join(target, filepath.FromSlash(rest))
		switch h.Typeflag {
		case tar.TypeDir:
			err = os.MkdirAll(p, 0o755)
		case tar.TypeReg:
			if err = os.MkdirAll(filepath.Dir(p), 0o755); err == nil {
				err = writeMode(p, tr, h.FileInfo().Mode().Perm())
				n++
			}
		}
		// ponytail: links are skipped: the VM is not trusted to aim them
		// (a link then a file through it could write outside target).
		if err != nil {
			return n, err
		}
	}
}

func orDot(s string) string {
	if s == "" {
		return "."
	}
	return s
}

func writeMode(p string, r io.Reader, mode os.FileMode) error {
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
