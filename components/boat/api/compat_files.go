package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// boat.dev's file operations on the native ones (agentops.go). Paths are
// boat.dev's: relative to /home/user, which is the guest user's home here.

// fileEntry is a native file_list entry as boat.dev's FileEntry.
func fileEntry(dir string, e map[string]any) map[string]any {
	name := str(e, "name")
	size, _ := e["size"].(float64)
	mtime, _ := e["mtime"].(float64)
	return map[string]any{"name": name, "path": path.Join(dir, name), "isDir": str(e, "type") == "dir",
		"isSymlink": str(e, "type") == "link", "size": int64(size), "mode": str(e, "mode"),
		"modifiedAt": time.Unix(int64(mtime), 0).UTC()}
}

// fileQuery calls a native file GET with ?path= (and more).
func (s *server) fileQuery(r *http.Request, p *principal, pattern string, h handlerFunc, q url.Values) *nativeAnswer {
	return s.callNative(r, p, pattern, h, r.PathValue("id"), nil, "?", q.Encode())
}

func (s *server) compatDeleteFile(w http.ResponseWriter, r *http.Request, p *principal) {
	if r.URL.Query().Get("path") == "" {
		writeErr(w, r, 400, "invalid_path", "path is required")
		return
	}
	dst := s.compatPath(r.URL.Query().Get("path"))
	q := url.Values{"path": {dst}}
	if r.URL.Query().Get("recursive") == "true" || r.URL.Query().Get("recursive") == "1" {
		q.Set("recursive", "1")
	}
	res := s.fileQuery(r, p, "DELETE /v1/vms/{id}/files", s.deleteFile, q)
	if !res.ok() {
		res.relay(w)
		return
	}
	writeCompat(w, 200, "file.deleted", map[string]any{"path": dst})
}

func (s *server) compatListFiles(w http.ResponseWriter, r *http.Request, p *principal) {
	dir := s.compatPath(r.URL.Query().Get("path"))
	res := s.fileQuery(r, p, "GET /v1/vms/{id}/files/list", s.listFiles, url.Values{"path": {dir}})
	if !res.ok() {
		res.relay(w)
		return
	}
	var list struct {
		Entries   []map[string]any `json:"entries"`
		Truncated bool             `json:"truncated"`
	}
	json.Unmarshal(res.body.Bytes(), &list)
	files := []map[string]any{}
	for _, e := range list.Entries {
		files = append(files, fileEntry(dir, e))
	}
	writeCompat(w, 200, "file.list", map[string]any{"path": dir, "files": files, "truncated": list.Truncated})
}

// statAnswer answers boat.dev's FileInfoResponse for p, as typ.
func (s *server) statAnswer(w http.ResponseWriter, r *http.Request, pr *principal, file, typ string) {
	res := s.fileQuery(r, pr, "GET /v1/vms/{id}/files/info", s.fileInfo, url.Values{"path": {file}})
	if !res.ok() {
		res.relay(w)
		return
	}
	var st map[string]any
	json.Unmarshal(res.body.Bytes(), &st)
	e := fileEntry(path.Dir(file), map[string]any{"name": path.Base(file), "type": st["type"], "size": st["size"],
		"mode": st["mode"], "mtime": st["mtime"]})
	writeCompat(w, 200, typ, e)
}

func (s *server) compatFileInfo(w http.ResponseWriter, r *http.Request, p *principal) {
	if r.URL.Query().Get("path") == "" {
		writeErr(w, r, 400, "invalid_path", "path is required")
		return
	}
	s.statAnswer(w, r, p, s.compatPath(r.URL.Query().Get("path")), "file.info")
}

func (s *server) compatSearchFiles(w http.ResponseWriter, r *http.Request, p *principal) {
	q := r.URL.Query()
	pattern := strings.Trim(q.Get("pattern"), "*")
	if pattern == "" {
		writeErr(w, r, 400, "invalid_request", "pattern is required")
		return
	}
	dir := s.compatPath(q.Get("path"))
	res := s.fileQuery(r, p, "GET /v1/vms/{id}/files/search", s.searchFiles, url.Values{"path": {dir}, "q": {pattern}, "limit": {"1000"}})
	if !res.ok() {
		res.relay(w)
		return
	}
	var list struct {
		Matches   []map[string]any `json:"matches"`
		Truncated bool             `json:"truncated"`
	}
	json.Unmarshal(res.body.Bytes(), &list)
	files := []string{}
	for _, m := range list.Matches {
		files = append(files, str(m, "path"))
	}
	writeCompat(w, 200, "file.search", map[string]any{"files": files, "truncated": list.Truncated})
}

func (s *server) compatMakeFolder(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Path string `json:"path"`
		Mode string `json:"mode"`
	}
	if !decode(w, r, &body) {
		return
	}
	dir := s.compatPath(body.Path)
	res := s.callNative(r, p, "POST /v1/vms/{id}/files/folder", s.makeFolder, r.PathValue("id"), map[string]any{"path": dir})
	if !res.ok() {
		res.relay(w)
		return
	}
	if body.Mode != "" {
		res = s.callNative(r, p, "POST /v1/vms/{id}/files/permissions", s.chmodFile, r.PathValue("id"), map[string]any{"path": dir, "mode": body.Mode})
		if !res.ok() {
			res.relay(w)
			return
		}
	}
	s.statAnswer(w, r, p, dir, "folder.created")
}

func (s *server) compatMoveFile(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Source      string `json:"source"`
		Destination string `json:"destination"`
		Overwrite   bool   `json:"overwrite"`
	}
	if !decode(w, r, &body) {
		return
	}
	from, to := s.compatPath(body.Source), s.compatPath(body.Destination)
	res := s.callNative(r, p, "POST /v1/vms/{id}/files/move", s.moveFile, r.PathValue("id"),
		map[string]any{"from": from, "to": to, "overwrite": body.Overwrite})
	if !res.ok() {
		res.relay(w)
		return
	}
	writeCompat(w, 200, "file.moved", map[string]any{"path": from, "destination": to})
}

func (s *server) compatChmod(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Path string `json:"path"`
		Mode string `json:"mode"`
	}
	if !decode(w, r, &body) {
		return
	}
	file := s.compatPath(body.Path)
	res := s.callNative(r, p, "POST /v1/vms/{id}/files/permissions", s.chmodFile, r.PathValue("id"), map[string]any{"path": file, "mode": body.Mode})
	if !res.ok() {
		res.relay(w)
		return
	}
	s.statAnswer(w, r, p, file, "file.permissions_set")
}
