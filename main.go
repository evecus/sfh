package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	dataRoot = "."
	authUser = ""
	authPass = ""
	port     = "4040"
)

const (
	maxUploadSize  = 10 << 30  // 10 GB total per request
	maxCatSize     = 10 << 20  // 10 MB for /api/cat
	maxZipWalkSize = 20 << 30  // 20 GB total for zip download
)

func parseArgs() {
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--auth" && i+1 < len(args):
			i++
			parts := strings.SplitN(args[i], ":", 2)
			if len(parts) == 2 {
				authUser = parts[0]
				authPass = parts[1]
			}
		case args[i] == "--port" && i+1 < len(args):
			i++
			port = args[i]
		case args[i] == "--dir" && i+1 < len(args):
			i++
			dataRoot = args[i]
		case !strings.HasPrefix(args[i], "--"):
			port = args[i]
		}
	}
}

// checkAuth validates Basic Auth credentials.
// If no --auth flag was provided, write operations are always denied.
// Pass requireCreds=false only for public read endpoints.
func checkAuth(r *http.Request, requireCreds bool) bool {
	if authUser == "" && authPass == "" {
		// No credentials configured: allow only if the caller explicitly
		// marks the endpoint as public (requireCreds == false).
		return !requireCreds
	}
	u, p, ok := r.BasicAuth()
	return ok && u == authUser && p == authPass
}

func requireAuth(w http.ResponseWriter, r *http.Request) bool {
	if checkAuth(r, true) {
		return true
	}
	w.Header().Set("WWW-Authenticate", `Basic realm="sfh"`)
	http.Error(w, "Unauthorized", 401)
	return false
}

func safePath(rel string) (string, bool) {
	rel = filepath.Clean("/" + rel)
	abs, err := filepath.Abs(filepath.Join(dataRoot, rel))
	if err != nil {
		return "", false
	}
	root, _ := filepath.Abs(dataRoot)
	if abs != root && !strings.HasPrefix(abs, root+string(filepath.Separator)) {
		return "", false
	}
	return abs, true
}

func fmtSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// isPreviewable: text/image files served raw by browser natively
func isPreviewable(ext string) bool {
	m := map[string]bool{
		// text
		".txt": true, ".md": true, ".json": true, ".yaml": true, ".yml": true,
		".toml": true, ".ini": true, ".cfg": true, ".conf": true, ".log": true,
		".sh": true, ".bash": true, ".zsh": true, ".go": true, ".py": true,
		".js": true, ".ts": true, ".jsx": true, ".tsx": true, ".html": true,
		".htm": true, ".css": true, ".xml": true, ".c": true, ".cpp": true,
		".h": true, ".rs": true, ".java": true, ".rb": true, ".php": true,
		".lua": true, ".env": true, ".csv": true, ".sql": true, ".lock": true,
		".gitignore": true, ".dockerfile": true,
		// image
		".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
		".webp": true, ".bmp": true, ".ico": true, ".svg": true,
	}
	return m[strings.ToLower(ext)]
}

func isBrowser(r *http.Request) bool {
	ua := strings.ToLower(r.Header.Get("User-Agent"))
	if ua == "" {
		return false
	}
	for _, c := range []string{"curl", "wget", "httpie", "python-requests", "go-http-client"} {
		if strings.Contains(ua, c) {
			return false
		}
	}
	return true
}

type Crumb struct{ Name, URL string }

func crumbs(path string) []Crumb {
	list := []Crumb{{"~", "/"}}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	cur := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		cur += "/" + p
		list = append(list, Crumb{p, cur})
	}
	return list
}

func updCrumbs(path string) []Crumb {
	list := []Crumb{{"~", "/update"}}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	cur := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		cur += "/" + p
		list = append(list, Crumb{p, "/update" + cur})
	}
	return list
}

var fm = template.FuncMap{
	"join": func(base, name string) string {
		return strings.TrimSuffix(base, "/") + "/" + name
	},
	"joinDir": func(base, name string) string {
		return strings.TrimSuffix(base, "/") + "/" + name + "/"
	},
	"js": func(v any) template.JS {
		b, _ := json.Marshal(v)
		return template.JS(b)
	},
	"not": func(b bool) bool { return !b },
}

// ─── public handler ──────────────────────────────────────────────────────────

func handler(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	abs, ok := safePath(p)
	if !ok {
		http.Error(w, "Forbidden", 403)
		return
	}
	info, err := os.Stat(abs)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if info.IsDir() {
		if !strings.HasSuffix(p, "/") {
			http.Redirect(w, r, p+"/", 302)
			return
		}
		if isBrowser(r) {
			dirPage(w, r, p, abs)
		} else {
			// curl/wget: plain text listing
			entries, _ := os.ReadDir(abs)
			var lines []string
			for _, e := range entries {
				if e.IsDir() {
					lines = append(lines, e.Name()+"/")
				} else {
					lines = append(lines, e.Name())
				}
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintln(w, strings.Join(lines, "\n"))
		}
		return
	}

	// For all files: serve raw content directly.
	// - Previewable files (text/image): browser renders natively.
	// - Other files: browser prompts download.
	// curl/wget always get raw content regardless.
	http.ServeFile(w, r, abs)
}

type FI struct {
	Name, Size, ModTime string
	IsDir               bool
}

func readDir(abs string) []FI {
	entries, _ := os.ReadDir(abs)
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
	})
	var out []FI
	for _, e := range entries {
		info, _ := e.Info()
		sz := ""
		if !e.IsDir() {
			sz = fmtSize(info.Size())
		}
		out = append(out, FI{e.Name(), sz, info.ModTime().Format("2006-01-02 15:04"), e.IsDir()})
	}
	return out
}

func dirPage(w http.ResponseWriter, r *http.Request, p, abs string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	t := template.Must(template.New("d").Funcs(fm).Parse(dirTmpl))
	t.Execute(w, map[string]any{
		"Path":   p,
		"Crumbs": crumbs(strings.TrimSuffix(p, "/")),
		"Files":  readDir(abs),
	})
}

// ─── update handler ──────────────────────────────────────────────────────────

func updateHandler(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r) {
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/update")
	if p == "" {
		p = "/"
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	abs, ok := safePath(p)
	if !ok {
		http.Error(w, "Forbidden", 403)
		return
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		http.Error(w, "Not found", 404)
		return
	}
	if r.Method == "POST" {
		updatePost(w, r, p, abs)
		return
	}
	updatePage(w, r, p, abs)
}

func updatePost(w http.ResponseWriter, r *http.Request, dir, absDir string) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "request too large or malformed", 400)
		return
	}
	action := r.FormValue("action")

	switch action {
	case "upload":
		if r.MultipartForm != nil {
			for _, fhs := range r.MultipartForm.File {
				for _, fh := range fhs {
					safeName := filepath.Base(fh.Filename)
					dst, ok := safePath(filepath.Join(dir, safeName))
					if !ok {
						continue
					}
					src, err := fh.Open()
					if err != nil {
						continue
					}
					f, err := os.Create(dst)
					if err == nil {
						io.Copy(f, src)
						f.Close()
					}
					src.Close()
				}
			}
		}
	case "upload_folder":
		if r.MultipartForm != nil {
			for _, fhs := range r.MultipartForm.File["files"] {
				// Sanitise the relative path supplied by the client.
				// filepath.FromSlash + Clean turns any "../.." into a
				// safe relative path, then safePath enforces the root boundary.
				relName := filepath.FromSlash(filepath.Clean("/" + fhs.Filename))
				dst, ok := safePath(filepath.Join(dir, relName))
				if !ok {
					continue // silently skip path-traversal attempts
				}
				if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
					continue
				}
				src, err := fhs.Open()
				if err != nil {
					continue
				}
				f, err := os.Create(dst)
				if err != nil {
					src.Close()
					continue
				}
				io.Copy(f, src)
				f.Close()
				src.Close()
			}
		}
	case "mkdir":
		name := strings.TrimSpace(r.FormValue("name"))
		if name != "" && !strings.ContainsAny(name, "/\\") {
			os.MkdirAll(filepath.Join(absDir, name), 0755)
		}
	case "delete":
		name := r.FormValue("name")
		if name != "" {
			target, ok := safePath(filepath.Join(dir, name))
			root, _ := filepath.Abs(dataRoot)
			if ok && target != root {
				os.RemoveAll(target)
			}
		}
	case "rename":
		old := r.FormValue("old")
		nw := r.FormValue("new")
		if old != "" && nw != "" && !strings.ContainsAny(nw, "/\\") {
			oldA, ok1 := safePath(filepath.Join(dir, old))
			newA, ok2 := safePath(filepath.Join(dir, nw))
			if ok1 && ok2 {
				os.Rename(oldA, newA)
			}
		}
	}
	http.Redirect(w, r, "/update"+dir, 303)
}

func updatePage(w http.ResponseWriter, r *http.Request, p, abs string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	t := template.Must(template.New("u").Funcs(fm).Parse(updTmpl))
	t.Execute(w, map[string]any{
		"Path":   p,
		"Crumbs": updCrumbs(strings.TrimSuffix(p, "/")),
		"Files":  readDir(abs),
	})
}

// ─── JSON API ─────────────────────────────────────────────────────────────────
//
// Public (no auth):
//   GET /api/ls[?path=/dir]           list directory
//   GET /api/cat?path=/file           read file text
//   GET /api/download?path=/file      download (dir → zip)
//
// Protected (Basic Auth):
//   POST   /api/upload?path=/dir[&name=x]   upload file
//   DELETE /api/delete?path=/x              delete
//   POST   /api/mkdir?path=/dir             create dir
//   POST   /api/move?from=/a&to=/b          move/rename

func apiHandler(w http.ResponseWriter, r *http.Request) {
	sub := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api"), "/")
	isWrite := r.Method == "POST" || r.Method == "DELETE" || r.Method == "PUT"
	if isWrite && !requireAuth(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	switch sub {
	case "ls", "":
		apiLS(w, r)
	case "cat":
		apiCat(w, r)
	case "upload":
		apiUpload(w, r)
	case "delete":
		apiDelete(w, r)
	case "mkdir":
		apiMkdir(w, r)
	case "move":
		apiMove(w, r)
	case "download":
		apiDownload(w, r)
	default:
		jErr(w, "unknown: "+sub, 404)
	}
}

func jOK(w http.ResponseWriter, d any) {
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": d})
}
func jErr(w http.ResponseWriter, msg string, code int) {
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg})
}
func qp(r *http.Request) string {
	p := r.URL.Query().Get("path")
	if p == "" {
		p = "/"
	}
	return p
}

func apiLS(w http.ResponseWriter, r *http.Request) {
	rel := qp(r)
	abs, ok := safePath(rel)
	if !ok {
		jErr(w, "invalid path", 400)
		return
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		jErr(w, err.Error(), 500)
		return
	}
	type AF struct {
		Name    string `json:"name"`
		Path    string `json:"path"`
		IsDir   bool   `json:"is_dir"`
		Size    int64  `json:"size,omitempty"`
		ModTime string `json:"mod_time"`
	}
	var out []AF
	for _, e := range entries {
		info, _ := e.Info()
		fp := strings.TrimSuffix(rel, "/") + "/" + e.Name()
		f := AF{Name: e.Name(), Path: fp, IsDir: e.IsDir(), ModTime: info.ModTime().Format(time.RFC3339)}
		if !e.IsDir() {
			f.Size = info.Size()
		}
		out = append(out, f)
	}
	if out == nil {
		out = []AF{}
	}
	jOK(w, out)
}

func apiCat(w http.ResponseWriter, r *http.Request) {
	abs, ok := safePath(qp(r))
	if !ok {
		jErr(w, "invalid path", 400)
		return
	}
	info, err := os.Stat(abs)
	if err != nil {
		jErr(w, err.Error(), 500)
		return
	}
	if info.Size() > maxCatSize {
		jErr(w, fmt.Sprintf("file too large for cat (max %s)", fmtSize(maxCatSize)), 400)
		return
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		jErr(w, err.Error(), 500)
		return
	}
	jOK(w, string(b))
}

func apiUpload(w http.ResponseWriter, r *http.Request) {
	dir := qp(r)
	absDir, ok := safePath(dir)
	if !ok {
		jErr(w, "invalid path", 400)
		return
	}
	os.MkdirAll(absDir, 0755)

	// Enforce a hard ceiling on the entire request body.
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)

	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			jErr(w, "request too large or malformed", 400)
			return
		}
		var up []string
		for _, fhs := range r.MultipartForm.File {
			for _, fh := range fhs {
				// Only use the base name — no subdirectory component.
				safeName := filepath.Base(fh.Filename)
				dst, ok2 := safePath(filepath.Join(dir, safeName))
				if !ok2 {
					continue
				}
				src, err := fh.Open()
				if err != nil {
					continue
				}
				f, err := os.Create(dst)
				if err == nil {
					io.Copy(f, src)
					f.Close()
					up = append(up, safeName)
				}
				src.Close()
			}
		}
		jOK(w, map[string]any{"uploaded": up})
		return
	}
	// Raw-body upload: validate the supplied name through safePath.
	name := filepath.Base(r.URL.Query().Get("name"))
	if name == "" || name == "." {
		name = "upload_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	dst, ok := safePath(filepath.Join(dir, name))
	if !ok {
		jErr(w, "invalid name", 400)
		return
	}
	f, err := os.Create(dst)
	if err != nil {
		jErr(w, err.Error(), 500)
		return
	}
	defer f.Close()
	n, _ := io.Copy(f, r.Body)
	jOK(w, map[string]any{"uploaded": name, "bytes": n})
}

func apiDelete(w http.ResponseWriter, r *http.Request) {
	abs, ok := safePath(qp(r))
	if !ok {
		jErr(w, "invalid path", 400)
		return
	}
	root, _ := filepath.Abs(dataRoot)
	if abs == root {
		jErr(w, "cannot delete root", 400)
		return
	}
	if err := os.RemoveAll(abs); err != nil {
		jErr(w, err.Error(), 500)
		return
	}
	jOK(w, "deleted")
}

func apiMkdir(w http.ResponseWriter, r *http.Request) {
	abs, ok := safePath(qp(r))
	if !ok {
		jErr(w, "invalid path", 400)
		return
	}
	if err := os.MkdirAll(abs, 0755); err != nil {
		jErr(w, err.Error(), 500)
		return
	}
	jOK(w, "created")
}

func apiMove(w http.ResponseWriter, r *http.Request) {
	absFrom, ok1 := safePath(r.URL.Query().Get("from"))
	absTo, ok2 := safePath(r.URL.Query().Get("to"))
	if !ok1 || !ok2 {
		jErr(w, "invalid path", 400)
		return
	}
	if err := os.Rename(absFrom, absTo); err != nil {
		jErr(w, err.Error(), 500)
		return
	}
	jOK(w, "moved")
}

func apiDownload(w http.ResponseWriter, r *http.Request) {
	abs, ok := safePath(qp(r))
	if !ok {
		http.Error(w, "invalid path", 400)
		return
	}
	info, err := os.Stat(abs)
	if err != nil {
		http.Error(w, "not found", 404)
		return
	}
	if info.IsDir() {
		w.Header().Set("Content-Disposition", `attachment; filename="`+info.Name()+`.zip"`)
		w.Header().Set("Content-Type", "application/zip")
		zw := zip.NewWriter(w)
		defer zw.Close()
		var totalBytes int64
		filepath.Walk(abs, func(path string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() {
				return nil
			}
			totalBytes += fi.Size()
			if totalBytes > maxZipWalkSize {
				return fmt.Errorf("zip size limit exceeded")
			}
			rel, _ := filepath.Rel(abs, path)
			fw, err := zw.Create(rel)
			if err != nil {
				return nil
			}
			f, err := os.Open(path)
			if err == nil {
				io.Copy(fw, f)
				f.Close()
			}
			return nil
		})
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+info.Name()+`"`)
	http.ServeFile(w, r, abs)
}

// ─── main ─────────────────────────────────────────────────────────────────────

func main() {
	parseArgs()
	abs, err := filepath.Abs(dataRoot)
	if err != nil {
		abs = "."
	}
	dataRoot = abs
	if err := os.MkdirAll(dataRoot, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "cannot create %s: %v\n", dataRoot, err)
		os.Exit(1)
	}

	http.HandleFunc("/api/", apiHandler)
	http.HandleFunc("/update", updateHandler)
	http.HandleFunc("/update/", updateHandler)
	http.HandleFunc("/", handler)

	auth := "none"
	if authUser != "" {
		auth = authUser + ":***"
	}
	fmt.Printf("\n  sfh — Static File Host\n\n")
	fmt.Printf("  📁 dir    %s\n  🌐 port   :%s\n  🔑 auth   %s\n\n", dataRoot, port, auth)
	if authUser == "" {
		fmt.Fprintf(os.Stderr, "  ⚠️  WARNING: no --auth set. /update and write API endpoints are DISABLED.\n")
		fmt.Fprintf(os.Stderr, "              Start with --auth user:pass to enable file management.\n\n")
	}
	fmt.Printf("  Browse → http://localhost:%s/\n", port)
	fmt.Printf("  Manage → http://localhost:%s/update  (needs auth)\n\n", port)
	fmt.Printf("  curl API (read — public):\n")
	fmt.Printf("    curl http://HOST:%s/api/ls\n", port)
	fmt.Printf("    curl http://HOST:%s/api/ls?path=/subdir\n", port)
	fmt.Printf("    curl http://HOST:%s/api/cat?path=/file.txt\n", port)
	fmt.Printf("    curl http://HOST:%s/api/download?path=/file.txt -o file.txt\n\n", port)
	if authUser != "" {
		fmt.Printf("  curl API (write — requires -u %s:PASS):\n", authUser)
		fmt.Printf("    curl -u %s:PASS -X POST 'http://HOST:%s/api/mkdir?path=/dir'\n", authUser, port)
		fmt.Printf("    curl -u %s:PASS -T file.txt 'http://HOST:%s/api/upload?path=/&name=file.txt'\n", authUser, port)
		fmt.Printf("    curl -u %s:PASS -F 'files=@photo.png' 'http://HOST:%s/api/upload?path=/'\n", authUser, port)
		fmt.Printf("    curl -u %s:PASS -X DELETE 'http://HOST:%s/api/delete?path=/file.txt'\n", authUser, port)
		fmt.Printf("    curl -u %s:PASS -X POST 'http://HOST:%s/api/move?from=/a.txt&to=/b.txt'\n\n", authUser, port)
	}

	if err := http.ListenAndServe(":"+port, nil); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// ─── templates ────────────────────────────────────────────────────────────────

// dirTmpl: public directory listing — clean white theme
const dirTmpl = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Path}} — sfh</title>
<link rel="icon" type="image/svg+xml" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'%3E%3Crect width='32' height='32' rx='7' fill='%232563eb'/%3E%3Ctext x='16' y='22' font-family='monospace' font-weight='700' font-size='14' fill='white' text-anchor='middle'%3Esfh%3C/text%3E%3C/svg%3E">
<style>
@import url('https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@400;500&family=Inter:wght@300;400;500&display=swap');
*{box-sizing:border-box;margin:0;padding:0}
:root{
  --bg:#f8f9fb;--surf:#ffffff;--bdr:#e5e8ef;--bdr2:#d1d5de;
  --acc:#2563eb;--grn:#059669;--txt:#111827;--sub:#6b7280;
  --hvr:#f1f4f9;
}
body{background:var(--bg);color:var(--txt);font-family:'Inter',sans-serif;min-height:100vh;font-size:14px}
header{
  background:var(--surf);border-bottom:1px solid var(--bdr);
  padding:0 28px;height:52px;display:flex;align-items:center;gap:14px;
  position:sticky;top:0;z-index:10;
}
.logo{font-family:'JetBrains Mono',monospace;font-weight:500;font-size:.95rem;color:var(--acc);letter-spacing:.02em;flex-shrink:0}
.logo em{color:var(--grn);font-style:normal}
.bc{flex:1;display:flex;align-items:center;gap:2px;font-family:'JetBrains Mono',monospace;font-size:.78rem;min-width:0;overflow:hidden}
.bc a{color:var(--acc);text-decoration:none;padding:3px 6px;border-radius:4px;transition:.12s;white-space:nowrap}
.bc a:hover{background:var(--hvr);color:var(--grn)}
.bc .s{color:var(--bdr2);padding:0 1px}
main{max-width:860px;margin:32px auto;padding:0 20px}
.toolbar{display:flex;align-items:center;justify-content:space-between;margin-bottom:12px}
.info{font-size:.75rem;font-family:'JetBrains Mono',monospace;color:var(--sub)}
.card{background:var(--surf);border:1px solid var(--bdr);border-radius:8px;overflow:hidden}
table{width:100%;border-collapse:collapse}
th{
  text-align:left;padding:8px 14px;font-size:.7rem;font-weight:500;
  color:var(--sub);text-transform:uppercase;letter-spacing:.08em;
  border-bottom:1px solid var(--bdr);background:#fafbfc;
  font-family:'JetBrains Mono',monospace;
}
td{padding:9px 14px;border-bottom:1px solid var(--bdr);vertical-align:middle}
tr:last-child td{border-bottom:none}
tr:hover td{background:var(--hvr)}
.nm-cell{display:flex;align-items:center;gap:8px}
.ic{font-size:.95rem;flex-shrink:0}
a.nm{color:var(--txt);text-decoration:none;font-size:.88rem;transition:.12s}
a.nm:hover{color:var(--acc)}
a.nm.dir{color:var(--acc);font-weight:500}
a.nm.dir:hover{color:var(--grn)}
.sz,.mt{color:var(--sub);font-family:'JetBrains Mono',monospace;font-size:.75rem;white-space:nowrap}
.rb{
  display:inline-block;font-size:.63rem;font-family:'JetBrains Mono',monospace;
  background:#eff2f7;color:var(--sub);padding:1px 5px;border-radius:3px;
  margin-left:4px;vertical-align:middle;border:1px solid var(--bdr);
}
.empty{text-align:center;padding:56px;color:var(--sub);font-family:'JetBrains Mono',monospace;font-size:.85rem}
</style>
</head>
<body>
<header>
  <span class="logo">sfh<em>.</em></span>
  <div class="bc">
    {{range $i,$c:=.Crumbs}}{{if $i}}<span class="s">/</span>{{end}}<a href="{{$c.URL}}">{{$c.Name}}</a>{{end}}
  </div>
</header>
<main>
  <div class="toolbar">
    <span class="info">{{len .Files}} items</span>
  </div>
  <div class="card">
    {{if .Files}}
    <table>
      <thead><tr><th>Name</th><th>Size</th><th>Modified</th></tr></thead>
      <tbody>
      {{range .Files}}<tr>
        <td>
          <div class="nm-cell">
            <span class="ic">{{if .IsDir}}📁{{else}}📄{{end}}</span>
            {{if .IsDir}}
              <a class="nm dir" href="{{joinDir $.Path .Name}}">{{.Name}}</a>
            {{else}}
              <a class="nm" href="{{join $.Path .Name}}">{{.Name}}</a>
              <span class="rb">raw</span>
            {{end}}
          </div>
        </td>
        <td class="sz">{{.Size}}</td>
        <td class="mt">{{.ModTime}}</td>
      </tr>{{end}}
      </tbody>
    </table>
    {{else}}<div class="empty">— empty directory —</div>{{end}}
  </div>
</main>
</body></html>`

// updTmpl: management interface — keeps dark theme for visual distinction
const updTmpl = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>manage {{.Path}} — sfh</title>
<link rel="icon" type="image/svg+xml" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'%3E%3Crect width='32' height='32' rx='7' fill='%232563eb'/%3E%3Ctext x='16' y='22' font-family='monospace' font-weight='700' font-size='14' fill='white' text-anchor='middle'%3Esfh%3C/text%3E%3C/svg%3E">
<style>
@import url('https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@400;500&family=Inter:wght@300;400;500&display=swap');
*{box-sizing:border-box;margin:0;padding:0}
:root{
  --bg:#f8f9fb;--surf:#ffffff;--s2:#f3f5f8;--bdr:#e5e8ef;--bdr2:#d1d5de;
  --acc:#2563eb;--grn:#059669;--txt:#111827;--sub:#6b7280;
  --hvr:#f1f4f9;--red:#dc2626;
}
body{background:var(--bg);color:var(--txt);font-family:'Inter',sans-serif;min-height:100vh;font-size:14px}
header{
  background:var(--surf);border-bottom:1px solid var(--bdr);
  padding:0 28px;height:52px;display:flex;align-items:center;gap:12px;
  position:sticky;top:0;z-index:10;
}
.logo{font-family:'JetBrains Mono',monospace;font-weight:500;font-size:.95rem;color:var(--acc)}
.logo em{color:var(--grn);font-style:normal}
.badge{font-size:.62rem;font-family:'JetBrains Mono',monospace;border:1px solid var(--acc);color:var(--acc);padding:2px 7px;border-radius:20px;opacity:.7}
.bc{flex:1;display:flex;align-items:center;gap:2px;font-family:'JetBrains Mono',monospace;font-size:.78rem;margin-left:4px;min-width:0;overflow:hidden}
.bc a{color:var(--acc);text-decoration:none;padding:3px 6px;border-radius:4px;transition:.12s;white-space:nowrap}
.bc a:hover{background:var(--hvr);color:var(--grn)}
.bc .s{color:var(--bdr2);padding:0 1px}
.vw{font-size:.75rem;font-family:'JetBrains Mono',monospace;border:1px solid var(--bdr2);color:var(--sub);padding:4px 11px;border-radius:5px;text-decoration:none;transition:.15s;white-space:nowrap;flex-shrink:0}
.vw:hover{border-color:var(--grn);color:var(--grn)}
.wrap{display:flex;gap:20px;max-width:1160px;margin:24px auto;padding:0 20px}
.side{width:248px;flex-shrink:0}
.main{flex:1;min-width:0}
.card{background:var(--surf);border:1px solid var(--bdr);border-radius:8px;padding:16px;margin-bottom:16px}
.card h3{font-size:.65rem;text-transform:uppercase;letter-spacing:.1em;color:var(--sub);font-family:'JetBrains Mono',monospace;margin-bottom:12px;font-weight:500}
.dz{
  border:2px dashed var(--bdr2);border-radius:6px;padding:20px 10px;
  text-align:center;cursor:pointer;color:var(--sub);font-size:.8rem;
  transition:.15s;position:relative;user-select:none;
}
.dz:hover,.dz.ov{border-color:var(--acc);color:var(--acc);background:rgba(37,99,235,.04)}
.dz .ic{font-size:1.6rem;display:block;margin-bottom:4px}
.dz input{position:absolute;inset:0;opacity:0;cursor:pointer;width:100%;height:100%}
input[type=text]{
  width:100%;background:var(--s2);border:1px solid var(--bdr);
  color:var(--txt);padding:7px 10px;border-radius:5px;
  font-family:'JetBrains Mono',monospace;font-size:.8rem;outline:none;transition:.12s;
}
input[type=text]:focus{border-color:var(--acc)}
.btn{display:inline-flex;align-items:center;gap:4px;padding:5px 12px;border-radius:5px;font-size:.76rem;font-family:'JetBrains Mono',monospace;cursor:pointer;border:none;transition:.15s;font-weight:500;text-decoration:none}
.bp{background:var(--acc);color:#fff}.bp:hover{opacity:.85}
.bg{background:var(--grn);color:#fff}.bg:hover{opacity:.85}
.bgh{background:transparent;border:1px solid var(--bdr2);color:var(--sub)}.bgh:hover{border-color:var(--acc);color:var(--acc)}
.br{background:transparent;border:1px solid var(--bdr2);color:var(--red)}.br:hover{background:rgba(220,38,38,.06)}
.row{display:flex;gap:7px;margin-top:10px}
.prog{height:2px;background:var(--bdr);border-radius:1px;margin-top:7px;display:none;overflow:hidden}
.prog .fill{height:100%;background:var(--grn);transition:width .2s;width:0}
#st,#fst{font-size:.7rem;font-family:'JetBrains Mono',monospace;color:var(--grn);min-height:1em;margin-top:5px}
table{width:100%;border-collapse:collapse}
th{text-align:left;padding:7px 12px;font-size:.65rem;font-weight:500;color:var(--sub);text-transform:uppercase;letter-spacing:.08em;border-bottom:1px solid var(--bdr);font-family:'JetBrains Mono',monospace;background:var(--s2)}
td{padding:8px 12px;border-bottom:1px solid var(--bdr);vertical-align:middle}
tr:last-child td{border-bottom:none}
tr:hover td{background:var(--hvr)}
.fn{display:flex;align-items:center;gap:7px}
a.nl{color:var(--acc);text-decoration:none;font-size:.86rem;transition:.12s}
a.nl:hover{color:var(--grn)}
.fname{font-size:.86rem;color:var(--txt)}
.sc,.sm{color:var(--sub);font-family:'JetBrains Mono',monospace;font-size:.72rem}
.acts{display:flex;gap:4px;justify-content:flex-end}
.rf{display:none;align-items:center;gap:5px}
.rf input{flex:1;padding:4px 7px;font-size:.76rem}
.empty{text-align:center;padding:40px;color:var(--sub);font-family:'JetBrains Mono',monospace;font-size:.82rem}
@media(max-width:680px){.wrap{flex-direction:column}.side{width:100%}}
</style>
</head>
<body>
<header>
  <span class="logo">sfh<em>.</em></span>
  <span class="badge">manage</span>
  <div class="bc">
    {{range $i,$c:=.Crumbs}}{{if $i}}<span class="s">/</span>{{end}}<a href="{{$c.URL}}">{{$c.Name}}</a>{{end}}
  </div>
  <a class="vw" href="{{.Path}}">👁 view</a>
</header>
<div class="wrap">
<aside class="side">
  <div class="card">
    <h3>Upload Files</h3>
    <div class="dz" id="dz1">
      <span class="ic">📂</span>
      Drop files here<br><small>or click to browse</small>
      <input type="file" id="fi" multiple>
    </div>
    <div class="prog" id="pb"><div class="fill" id="pf"></div></div>
    <div id="st"></div>
  </div>
  <div class="card">
    <h3>Upload Folder</h3>
    <div class="dz" id="dz2">
      <span class="ic">📁</span>
      Drop folder here<br><small>or click to browse</small>
      <input type="file" id="ffi" webkitdirectory multiple>
    </div>
    <div id="fst"></div>
  </div>
  <div class="card">
    <h3>New Folder</h3>
    <form method="POST">
      <input type="hidden" name="action" value="mkdir">
      <input type="text" name="name" placeholder="folder name" required>
      <div class="row"><button class="btn bg" type="submit">＋ Create</button></div>
    </form>
  </div>
</aside>
<main class="main">
  <div class="card">
    <h3>{{.Path}}</h3>
    {{if .Files}}
    <table>
      <thead><tr><th>Name</th><th>Size</th><th>Modified</th><th></th></tr></thead>
      <tbody>
      {{range .Files}}<tr>
        <td>
          <div class="fn" id="fn-{{.Name}}">
            <span>{{if .IsDir}}📁{{else}}📄{{end}}</span>
            {{if .IsDir}}
              <a class="nl" href="/update{{joinDir $.Path .Name}}">{{.Name}}</a>
            {{else}}
              <span class="fname">{{.Name}}</span>
            {{end}}
          </div>
          <div class="rf" id="rf-{{.Name}}">
            <input type="text" id="rn-{{.Name}}" value="{{.Name}}">
            <button class="btn bp" style="padding:3px 8px" onclick="doRename('{{.Name}}')">✓</button>
            <button class="btn bgh" style="padding:3px 8px" onclick="cancelRename('{{.Name}}')">✕</button>
          </div>
        </td>
        <td class="sc">{{.Size}}</td>
        <td class="sm">{{.ModTime}}</td>
        <td><div class="acts">
          {{if not .IsDir}}<a href="{{join $.Path .Name}}" download class="btn bgh" style="padding:3px 8px;font-size:.7rem">⬇</a>{{end}}
          <button class="btn bgh" style="padding:3px 8px;font-size:.7rem" onclick="showRename('{{.Name}}')">✏</button>
          <button class="btn br" style="padding:3px 8px;font-size:.7rem" onclick="doDelete('{{.Name}}')">🗑</button>
        </div></td>
      </tr>{{end}}
      </tbody>
    </table>
    {{else}}<div class="empty">No files yet.</div>{{end}}
  </div>
</main>
</div>

<form id="df" method="POST" style="display:none">
  <input type="hidden" name="action" value="delete">
  <input type="hidden" name="name" id="dn">
</form>
<form id="rf2" method="POST" style="display:none">
  <input type="hidden" name="action" value="rename">
  <input type="hidden" name="old" id="ro">
  <input type="hidden" name="new" id="rn2">
</form>

<script>
const CUR={{js .Path}};
const dz1=document.getElementById('dz1'),fi=document.getElementById('fi');
const pb=document.getElementById('pb'),pf=document.getElementById('pf'),st=document.getElementById('st');
fi.addEventListener('change',()=>uploadFiles(fi.files));
dz1.addEventListener('dragover',e=>{e.preventDefault();dz1.classList.add('ov')});
dz1.addEventListener('dragleave',()=>dz1.classList.remove('ov'));
dz1.addEventListener('drop',e=>{e.preventDefault();dz1.classList.remove('ov');uploadFiles(e.dataTransfer.files)});
function uploadFiles(files){
  if(!files.length)return;
  const fd=new FormData();
  for(const f of files)fd.append('files',f);
  pb.style.display='block';st.textContent='Uploading...';
  const xhr=new XMLHttpRequest();
  xhr.open('POST','/api/upload?path='+encodeURIComponent(CUR));
  xhr.upload.onprogress=e=>{if(e.lengthComputable)pf.style.width=(e.loaded/e.total*100)+'%'};
  xhr.onload=()=>{pb.style.display='none';st.textContent='✓ Done';setTimeout(()=>location.reload(),500)};
  xhr.onerror=()=>{st.textContent='✗ Error'};
  xhr.send(fd);
}
const dz2=document.getElementById('dz2'),ffi=document.getElementById('ffi'),fst=document.getElementById('fst');
ffi.addEventListener('change',()=>uploadFolder(ffi.files));
dz2.addEventListener('dragover',e=>{e.preventDefault();dz2.classList.add('ov')});
dz2.addEventListener('dragleave',()=>dz2.classList.remove('ov'));
dz2.addEventListener('drop',e=>{
  e.preventDefault();dz2.classList.remove('ov');
  const allFiles=[];let pending=0;
  function traverse(entry,path){
    if(entry.isFile){
      pending++;
      entry.file(f=>{allFiles.push(new File([f],path+f.name,{type:f.type}));pending--;if(pending===0)uploadFolder(allFiles)});
    }else if(entry.isDirectory){
      const r=entry.createReader();pending++;
      r.readEntries(es=>{pending--;es.forEach(e2=>traverse(e2,path+entry.name+'/'));if(pending===0&&allFiles.length)uploadFolder(allFiles);});
    }
  }
  for(const item of e.dataTransfer.items){const en=item.webkitGetAsEntry();if(en)traverse(en,'')}
});
async function uploadFolder(files){
  if(!files.length)return;
  fst.textContent='Uploading...';
  const fd=new FormData();
  for(const f of files)fd.append('files',f,f.name);
  try{
    await fetch('/api/upload?path='+encodeURIComponent(CUR),{method:'POST',body:fd});
    fst.textContent='✓ Done';setTimeout(()=>location.reload(),500);
  }catch(e){fst.textContent='✗ '+e.message}
}
function doDelete(name){if(!confirm('Delete "'+name+'"?'))return;document.getElementById('dn').value=name;document.getElementById('df').submit()}
function showRename(name){document.getElementById('fn-'+name).style.display='none';document.getElementById('rf-'+name).style.display='flex'}
function cancelRename(name){document.getElementById('fn-'+name).style.display='flex';document.getElementById('rf-'+name).style.display='none'}
function doRename(old){
  const nw=document.getElementById('rn-'+old).value.trim();
  if(!nw||nw===old){cancelRename(old);return}
  document.getElementById('ro').value=old;document.getElementById('rn2').value=nw;document.getElementById('rf2').submit();
}
</script>
</body></html>`
