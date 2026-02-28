package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// allowedRoots are the only directory prefixes under which file operations are
// permitted.  Paths outside these roots get a 403 Forbidden response.
var allowedRoots = []string{"/workspace", "/tmp", "/var/tmp", "/home/runner"}

// validatePath checks that p is absolute, resolves symlinks, and verifies the
// resolved path falls under one of allowedRoots.  It returns the resolved
// absolute path or an error string + HTTP status code.
func validatePath(p string) (string, int, string) {
	if !filepath.IsAbs(p) {
		return "", http.StatusBadRequest, "path must be absolute"
	}

	// Resolve symlinks.  If the target doesn't exist yet (e.g. for write/mkdir),
	// resolve the parent directory instead and re-append the base name.
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		// Target may not exist yet — try resolving the parent.
		dir := filepath.Dir(p)
		resolvedDir, err2 := filepath.EvalSymlinks(dir)
		if err2 != nil {
			return "", http.StatusBadRequest, "cannot resolve path: " + err.Error()
		}
		resolved = filepath.Join(resolvedDir, filepath.Base(p))
	}

	for _, root := range allowedRoots {
		if strings.HasPrefix(resolved, root+"/") || resolved == root {
			return resolved, 0, ""
		}
	}
	return "", http.StatusForbidden, "path outside allowed roots"
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func jsonOK(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// ---------------------------------------------------------------------------
// POST /files/read
// ---------------------------------------------------------------------------

func fileReadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Path   string `json:"path"`
		Offset *int64 `json:"offset,omitempty"`
		Limit  *int64 `json:"limit,omitempty"`
		Base64 bool   `json:"base64,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}

	resolved, code, msg := validatePath(req.Path)
	if code != 0 {
		jsonError(w, code, msg)
		return
	}

	f, err := os.Open(resolved)
	if err != nil {
		jsonError(w, http.StatusNotFound, err.Error())
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if info.IsDir() {
		jsonError(w, http.StatusBadRequest, "path is a directory, use /files/list")
		return
	}

	if req.Offset != nil && *req.Offset > 0 {
		if _, err := f.Seek(*req.Offset, io.SeekStart); err != nil {
			jsonError(w, http.StatusInternalServerError, "seek failed: "+err.Error())
			return
		}
	}

	var reader io.Reader = f
	if req.Limit != nil && *req.Limit > 0 {
		reader = io.LimitReader(f, *req.Limit)
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "read failed: "+err.Error())
		return
	}

	var content string
	if req.Base64 {
		content = base64.StdEncoding.EncodeToString(data)
	} else {
		content = string(data)
	}

	jsonOK(w, map[string]interface{}{
		"path":   resolved,
		"size":   info.Size(),
		"content": content,
		"base64": req.Base64,
	})
}

// ---------------------------------------------------------------------------
// POST /files/write
// ---------------------------------------------------------------------------

func fileWriteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Base64  bool   `json:"base64,omitempty"`
		Mode    string `json:"mode,omitempty"`   // "overwrite" (default), "append"
		Perm    *int   `json:"perm,omitempty"`   // octal file permission, e.g. 0644
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}

	resolved, code, msg := validatePath(req.Path)
	if code != 0 {
		jsonError(w, code, msg)
		return
	}

	var data []byte
	var err error
	if req.Base64 {
		data, err = base64.StdEncoding.DecodeString(req.Content)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "base64 decode failed: "+err.Error())
			return
		}
	} else {
		data = []byte(req.Content)
	}

	perm := fs.FileMode(0644)
	if req.Perm != nil {
		perm = fs.FileMode(*req.Perm)
	}

	flag := os.O_WRONLY | os.O_CREATE
	if req.Mode == "append" {
		flag |= os.O_APPEND
	} else {
		flag |= os.O_TRUNC
	}

	f, err := os.OpenFile(resolved, flag, perm)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	n, err := f.Write(data)
	f.Close()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "write failed: "+err.Error())
		return
	}

	jsonOK(w, map[string]interface{}{
		"path":          resolved,
		"bytes_written": n,
	})
}

// ---------------------------------------------------------------------------
// POST /files/list
// ---------------------------------------------------------------------------

func fileListHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}

	resolved, code, msg := validatePath(req.Path)
	if code != 0 {
		jsonError(w, code, msg)
		return
	}

	type entry struct {
		Name    string `json:"name"`
		Path    string `json:"path"`
		Size    int64  `json:"size"`
		Mode    string `json:"mode"`
		ModTime string `json:"mod_time"`
		IsDir   bool   `json:"is_dir"`
	}

	var entries []entry

	if req.Recursive {
		err := filepath.WalkDir(resolved, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // skip errors
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			entries = append(entries, entry{
				Name:    d.Name(),
				Path:    p,
				Size:    info.Size(),
				Mode:    info.Mode().String(),
				ModTime: info.ModTime().UTC().Format(time.RFC3339),
				IsDir:   d.IsDir(),
			})
			return nil
		})
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else {
		dirEntries, err := os.ReadDir(resolved)
		if err != nil {
			jsonError(w, http.StatusNotFound, err.Error())
			return
		}
		for _, d := range dirEntries {
			info, err := d.Info()
			if err != nil {
				continue
			}
			entries = append(entries, entry{
				Name:    d.Name(),
				Path:    filepath.Join(resolved, d.Name()),
				Size:    info.Size(),
				Mode:    info.Mode().String(),
				ModTime: info.ModTime().UTC().Format(time.RFC3339),
				IsDir:   d.IsDir(),
			})
		}
	}

	if entries == nil {
		entries = []entry{}
	}

	jsonOK(w, map[string]interface{}{
		"path":    resolved,
		"entries": entries,
	})
}

// ---------------------------------------------------------------------------
// POST /files/stat
// ---------------------------------------------------------------------------

func fileStatHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}

	resolved, code, msg := validatePath(req.Path)
	if code != 0 {
		jsonError(w, code, msg)
		return
	}

	info, err := os.Stat(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			jsonOK(w, map[string]interface{}{
				"name":   filepath.Base(req.Path),
				"path":   resolved,
				"size":   0,
				"mode":   "",
				"mod_time": "",
				"is_dir": false,
				"exists": false,
			})
			return
		}
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	jsonOK(w, map[string]interface{}{
		"name":     info.Name(),
		"path":     resolved,
		"size":     info.Size(),
		"mode":     info.Mode().String(),
		"mod_time": info.ModTime().UTC().Format(time.RFC3339),
		"is_dir":   info.IsDir(),
		"exists":   true,
	})
}

// ---------------------------------------------------------------------------
// POST /files/remove
// ---------------------------------------------------------------------------

func fileRemoveHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}

	resolved, code, msg := validatePath(req.Path)
	if code != 0 {
		jsonError(w, code, msg)
		return
	}

	var err error
	if req.Recursive {
		err = os.RemoveAll(resolved)
	} else {
		err = os.Remove(resolved)
	}
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	jsonOK(w, map[string]interface{}{
		"path":    resolved,
		"removed": true,
	})
}

// ---------------------------------------------------------------------------
// POST /files/mkdir
// ---------------------------------------------------------------------------

func fileMkdirHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Path string `json:"path"`
		Perm *int   `json:"perm,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}

	resolved, code, msg := validatePath(req.Path)
	if code != 0 {
		jsonError(w, code, msg)
		return
	}

	perm := fs.FileMode(0755)
	if req.Perm != nil {
		perm = fs.FileMode(*req.Perm)
	}

	if err := os.MkdirAll(resolved, perm); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	jsonOK(w, map[string]interface{}{
		"path":    resolved,
		"created": true,
	})
}

// registerFileHandlers wires up all /files/* endpoints on the default mux.
func registerFileHandlers() {
	http.HandleFunc("/files/read", fileReadHandler)
	http.HandleFunc("/files/write", fileWriteHandler)
	http.HandleFunc("/files/list", fileListHandler)
	http.HandleFunc("/files/stat", fileStatHandler)
	http.HandleFunc("/files/remove", fileRemoveHandler)
	http.HandleFunc("/files/mkdir", fileMkdirHandler)
}
