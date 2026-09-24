// Package webui serves the embedded local web UI: a dependency-free static app
// (HTML, CSS, vanilla JavaScript modules) that talks to the management API
// under /api/v1 on the same listener using relative URLs. It makes no external
// requests, so it works offline and leaks nothing.
package webui

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed static
var staticFS embed.FS

// ContentSecurityPolicy forbids inline scripts and styles and every origin
// but our own. Scripts set element styles only through the CSSOM, which CSP
// permits.
const ContentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; " +
	"img-src 'self' data:; connect-src 'self'; font-src 'self'; object-src 'none'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Explicit types: Go's mime table can be overridden by the Windows registry,
// which has been known to map .js to text/plain.
var contentTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".svg":  "image/svg+xml",
	".json": "application/json",
	".txt":  "text/plain; charset=utf-8",
}

type asset struct {
	data  []byte
	ctype string
	etag  string
}

type handler struct{ assets map[string]asset }

// Handler returns the UI handler. Mount it at "/"; "/" serves index.html and
// other paths serve embedded assets by name. Anything not embedded is 404.
func Handler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	h := &handler{assets: map[string]asset{}}
	err = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(sub, p)
		if err != nil {
			return err
		}
		ct, ok := contentTypes[path.Ext(p)]
		if !ok {
			ct = "application/octet-stream"
		}
		sum := sha256.Sum256(b)
		h.assets[p] = asset{data: b, ctype: ct, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
		return nil
	})
	if err != nil {
		panic(err)
	}
	return h
}

func setSecurityHeaders(w http.ResponseWriter) {
	hd := w.Header()
	hd.Set("Content-Security-Policy", ContentSecurityPolicy)
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Referrer-Policy", "no-referrer")
	hd.Set("X-Frame-Options", "DENY")
	hd.Set("Cross-Origin-Opener-Policy", "same-origin")
	hd.Set("Cross-Origin-Resource-Policy", "same-origin")
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if p == "" {
		p = "index.html"
	}
	a, ok := h.assets[p]
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", a.ctype)
	w.Header().Set("ETag", a.etag)
	// Revalidate every time so an upgraded service never serves a stale UI;
	// the ETag makes that a cheap 304.
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(a.data))
}
