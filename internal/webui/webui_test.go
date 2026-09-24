package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"strings"
	"testing"
)

func serve(t *testing.T, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, req)
	return rec
}

func checkSecurityHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	want := map[string]string{
		"Content-Security-Policy": ContentSecurityPolicy,
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
		"X-Frame-Options":         "DENY",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestCSPIsStrict(t *testing.T) {
	for _, bad := range []string{"unsafe-inline", "unsafe-eval", "http:", "https:", "*"} {
		if strings.Contains(ContentSecurityPolicy, bad) {
			t.Errorf("CSP contains %q", bad)
		}
	}
	if !strings.Contains(ContentSecurityPolicy, "default-src 'self'") {
		t.Error("CSP lacks default-src 'self'")
	}
}

func TestServesIndexAndAssets(t *testing.T) {
	cases := []struct{ target, ctype, contains string }{
		{"/", "text/html; charset=utf-8", "<title>Benchwarmer</title>"},
		{"/index.html", "text/html; charset=utf-8", "js/app.js"},
		{"/app.css", "text/css; charset=utf-8", ":root"},
		{"/js/app.js", "text/javascript; charset=utf-8", "import"},
		{"/js/format.js", "text/javascript; charset=utf-8", "export function parseGoDuration"},
		{"/favicon.svg", "image/svg+xml", "<svg"},
	}
	for _, c := range cases {
		rec := serve(t, http.MethodGet, c.target, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", c.target, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); got != c.ctype {
			t.Errorf("%s: Content-Type %q, want %q", c.target, got, c.ctype)
		}
		if !strings.Contains(rec.Body.String(), c.contains) {
			t.Errorf("%s: body lacks %q", c.target, c.contains)
		}
		if rec.Header().Get("Cache-Control") != "no-cache" || rec.Header().Get("ETag") == "" {
			t.Errorf("%s: missing revalidation headers", c.target)
		}
		checkSecurityHeaders(t, rec)
	}
}

func TestNotFoundAndMethods(t *testing.T) {
	for _, target := range []string{"/missing.js", "/js/", "/js", "/static/index.html", "/../webui.go", "/events"} {
		rec := serve(t, http.MethodGet, target, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", target, rec.Code)
		}
		checkSecurityHeaders(t, rec)
	}
	rec := serve(t, http.MethodPost, "/", nil)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("POST /: status %d allow %q", rec.Code, rec.Header().Get("Allow"))
	}
	if rec := serve(t, http.MethodHead, "/app.css", nil); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("HEAD: status %d body %d", rec.Code, rec.Body.Len())
	}
}

func TestETagRevalidation(t *testing.T) {
	etag := serve(t, http.MethodGet, "/js/app.js", nil).Header().Get("ETag")
	rec := serve(t, http.MethodGet, "/js/app.js", map[string]string{"If-None-Match": etag})
	if rec.Code != http.StatusNotModified {
		t.Errorf("status %d, want 304", rec.Code)
	}
}

func embedded(t *testing.T) map[string]string {
	t.Helper()
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	err = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(sub, p)
		files[p] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// TestEmbeddedFilesAreConsistent stands in for a JS toolchain: every asset
// index.html references and every relative module import exists, and nothing
// needs inline script or style, an external origin, or contains an em-dash.
func TestEmbeddedFilesAreConsistent(t *testing.T) {
	files := embedded(t)
	index := files["index.html"]
	for _, m := range regexp.MustCompile(`(?:src|href)="([^"#]+)"`).FindAllStringSubmatch(index, -1) {
		if _, ok := files[m[1]]; !ok {
			t.Errorf("index.html references missing %s", m[1])
		}
	}
	if regexp.MustCompile(`<script(?:\s[^>]*)?>\s*[^<\s]`).MatchString(index) ||
		regexp.MustCompile(`<script(?:(?:\s(?:type|nonce)="[^"]*")*)\s*>`).MatchString(index) {
		t.Error("index.html has an inline script")
	}
	if strings.Contains(index, "<style") || regexp.MustCompile(`\sstyle=`).MatchString(index) || regexp.MustCompile(`\son[a-z]+=`).MatchString(index) {
		t.Error("index.html has inline style or event handler attributes")
	}
	importRe := regexp.MustCompile(`(?m)^\s*(?:import|export)\s[^;]*?from\s+"([^"]+)"`)
	external := regexp.MustCompile(`https?://`)
	for name, body := range files {
		if strings.ContainsRune(body, '\u2014') {
			t.Errorf("%s contains an em-dash", name)
		}
		for _, u := range external.FindAllStringIndex(body, -1) {
			rest := body[u[0]:]
			if !strings.HasPrefix(rest, "http://www.w3.org/2000/svg") {
				t.Errorf("%s references an external URL: %.40s", name, rest)
			}
		}
		if !strings.HasSuffix(name, ".js") {
			continue
		}
		if regexp.MustCompile(`localStorage\s*[.[]`).MatchString(body) {
			t.Errorf("%s uses localStorage", name)
		}
		if strings.Contains(body, ".innerHTML") || strings.Contains(body, "eval(") {
			t.Errorf("%s uses innerHTML or eval", name)
		}
		for _, m := range importRe.FindAllStringSubmatch(body, -1) {
			target := path.Join(path.Dir(name), m[1])
			if !strings.HasPrefix(m[1], "./") && !strings.HasPrefix(m[1], "../") {
				t.Errorf("%s imports non-relative %q", name, m[1])
			} else if _, ok := files[target]; !ok {
				t.Errorf("%s imports missing %s", name, target)
			}
		}
		if n := strings.Count(body, "{") - strings.Count(body, "}"); n != 0 {
			t.Errorf("%s has unbalanced braces (%+d)", name, n)
		}
	}
}
