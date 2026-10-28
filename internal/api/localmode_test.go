package api

import (
	"net/http"
	"strings"
	"testing"
)

// modePut is a same-origin dashboard request: what the web UI sends when it
// has no token.
func modePut(body string) req {
	return req{method: "PUT", path: "/api/v1/mode", body: body, json: true,
		headers: map[string]string{"Origin": "http://127.0.0.1:8481", "Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty"}}
}

func TestTokenlessModeFromThisPC(t *testing.T) {
	cases := []struct {
		name string
		mut  func(r *req)
	}{
		{"dashboard on 127.0.0.1", func(r *req) {}},
		{"dashboard on localhost", func(r *req) {
			r.host = "localhost:8481"
			r.headers["Origin"] = "http://localhost:8481"
		}},
		{"dashboard on IPv6 loopback", func(r *req) {
			r.peer, r.host = "[::1]:50000", "[::1]:8481"
			r.headers["Origin"] = "http://[::1]:8481"
		}},
		{"Origin differs only in case", func(r *req) { r.host = "LocalHost:8481"; r.headers["Origin"] = "http://localhost:8481" }},
		{"native client: no Origin, no Fetch Metadata", func(r *req) { r.headers = nil }},
		{"charset parameter on the content type", func(r *req) { r.ctype = "application/json; charset=utf-8" }},
	}
	for _, c := range cases {
		for _, body := range []string{`{"mode":"pause","duration":"30m","set_by":"ui"}`, `{"mode":"auto"}`} {
			t.Run(c.name+" "+body, func(t *testing.T) {
				hr := newHarness(t)
				r := modePut(body)
				c.mut(&r)
				rec := hr.do(r)
				if rec.Code != 200 {
					t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
				}
				m := hr.be.modes[0]
				if m.SetBy != "local" || m.Principal != "local" {
					t.Errorf("audit attribution: set_by=%q principal=%q, want local for both", m.SetBy, m.Principal)
				}
			})
		}
	}
}

func TestTokenlessModeKeepsLimits(t *testing.T) {
	hr := newHarness(t)
	for body, want := range map[string]int{
		`{"mode":"pause","duration":"720h"}`:         200,
		`{"mode":"pause","duration":"721h"}`:         400,
		`{"mode":"pause","duration":"soon"}`:         400,
		`{"mode":"auto","duration":"1h"}`:            400,
		`{"mode":"turbo"}`:                           400,
		`{"mode":"pause","duraton":"1h"}`:            400,
		`{"mode":"pause","duration":"-5m"}`:          400,
		`{"mode":"pause","duration":"until_reboot"}`: 200,
	} {
		if rec := hr.do(modePut(body)); rec.Code != want {
			t.Errorf("%s: got %d want %d: %s", body, rec.Code, want, rec.Body.String())
		}
	}
	// A backend refusal (a safety override) reaches the caller unchanged.
	hr.be.failWith = Rejectf("not allowed while the GPU is too hot")
	if rec := hr.do(modePut(`{"mode":"pause"}`)); rec.Code != 409 {
		t.Errorf("backend refusal: %d", rec.Code)
	}
}

func TestTokenlessAIPriorityNeedsToken(t *testing.T) {
	hr := newHarness(t)
	rec := hr.do(modePut(`{"mode":"ai_priority","duration":"1h"}`))
	if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("got %d (%q): %s", rec.Code, rec.Header().Get("WWW-Authenticate"), rec.Body.String())
	}
	if len(hr.be.modes) != 0 {
		t.Fatal("AI Priority reached the backend without a token")
	}
	// The same request with each accepted token goes through, and the audit
	// record names the token's holder, not the body's set_by.
	for tok, want := range map[string]string{mgmtTok: "management", agentTok: "agent"} {
		r := modePut(`{"mode":"ai_priority","duration":"1h","set_by":"tray"}`)
		r.token = tok
		if rec := hr.do(r); rec.Code != 200 {
			t.Fatalf("%s token: %d %s", want, rec.Code, rec.Body.String())
		}
		if got := hr.be.modes[len(hr.be.modes)-1]; got.Principal != want || got.SetBy != "tray" {
			t.Errorf("%s token: %+v", want, got)
		}
	}
}

// A tokenless request that a hostile page could produce must never reach the
// backend, whatever it is aimed at.
func TestTokenlessModeBarrier(t *testing.T) {
	body := `{"mode":"pause","duration":"8h"}`
	cases := []struct {
		name string
		mut  func(r *req)
		want int
	}{
		{"cross-origin fetch", func(r *req) {
			r.headers = map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "cors"}
		}, 403},
		{"cross-origin, no Fetch Metadata (older browser)", func(r *req) { r.headers = map[string]string{"Origin": "https://evil.example"} }, 403},
		{"another local port", func(r *req) { r.headers["Origin"] = "http://127.0.0.1:9999" }, 403},
		{"another local host name", func(r *req) { r.headers["Origin"] = "http://localhost:8481" }, 403},
		{"https origin on a plain listener", func(r *req) { r.headers["Origin"] = "https://127.0.0.1:8481" }, 403},
		{"Origin null (sandboxed frame, file page)", func(r *req) { r.headers["Origin"] = "null" }, 403},
		{"Origin with a path", func(r *req) { r.headers["Origin"] = "http://127.0.0.1:8481/x" }, 403},
		{"Origin with credentials", func(r *req) { r.headers["Origin"] = "http://a@127.0.0.1:8481" }, 403},
		{"two Origin headers", func(r *req) {
			r.addHeaders = map[string]string{"Origin": "https://evil.example"}
		}, 403},
		{"same-site, not same-origin", func(r *req) { r.headers["Sec-Fetch-Site"] = "same-site" }, 403},
		{"user navigation", func(r *req) { r.headers["Sec-Fetch-Site"] = "none" }, 403},
		{"no-cors mode", func(r *req) { r.headers["Sec-Fetch-Mode"] = "no-cors" }, 403},
		{"navigate mode", func(r *req) { r.headers["Sec-Fetch-Mode"] = "navigate" }, 403},
		{"frame destination", func(r *req) { r.headers["Sec-Fetch-Dest"] = "iframe" }, 403},
		{"browser metadata but no Origin", func(r *req) { delete(r.headers, "Origin") }, 403},
		{"HTML form post", func(r *req) { r.ctype = "application/x-www-form-urlencoded" }, 403},
		{"multipart form post", func(r *req) { r.ctype = "multipart/form-data; boundary=x" }, 403},
		{"text/plain simple request", func(r *req) { r.ctype = "text/plain" }, 403},
		{"no content type", func(r *req) { r.json, r.ctype = false, "" }, 403},
		{"json lookalike", func(r *req) { r.ctype = "application/jsonx" }, 403},
		{"DNS rebinding: attacker host name", func(r *req) {
			r.host = "evil.example:8481"
			r.headers["Origin"] = "http://evil.example:8481"
		}, 401},
		{"DNS rebinding: attacker host, no Origin", func(r *req) { r.host, r.headers = "evil.example:8481", nil }, 401},
		{"Host spoofed to localhost from a remote peer", func(r *req) { r.peer, r.host, r.headers = remote, "localhost:8481", nil }, 401},
		{"remote peer", func(r *req) { r.peer, r.headers = remote, nil }, 401},
		{"forwarded header claims loopback", func(r *req) {
			r.peer, r.host, r.headers = remote, "localhost:8481", map[string]string{"X-Forwarded-For": "127.0.0.1", "X-Real-IP": "127.0.0.1"}
		}, 401},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hr := newHarness(t)
			r := modePut(body)
			c.mut(&r)
			rec := hr.do(r)
			if rec.Code != c.want {
				t.Fatalf("got %d want %d: %s", rec.Code, c.want, rec.Body.String())
			}
			decodeErr(t, rec)
			if len(hr.be.modes) != 0 {
				t.Fatal("mode change reached the backend")
			}
		})
	}
}

func TestTokenlessModeRefusedWhenLoopbackTrustOff(t *testing.T) {
	hr := newHarness(t)
	hr.be.cfg.Security.LoopbackTrust = false
	rec := hr.do(modePut(`{"mode":"pause"}`))
	if rec.Code != 401 || len(hr.be.modes) != 0 {
		t.Fatalf("got %d with trust off: %s", rec.Code, rec.Body.String())
	}
	// Tokens are unaffected by the setting.
	r := modePut(`{"mode":"pause"}`)
	r.token = agentTok
	if rec := hr.do(r); rec.Code != 200 {
		t.Fatalf("agent token with trust off: %d", rec.Code)
	}
}

// The barrier applies only to a tokenless request: a bearer token cannot be
// sent by a page that does not hold it, so token paths keep working from any
// client, and a bad token is never rescued by being "local".
func TestModeBearerPathsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(r *req)
		want int
	}{
		{"management token, no browser headers, text/plain", func(r *req) { r.token, r.headers, r.json, r.ctype = mgmtTok, nil, false, "text/plain" }, 200},
		{"agent token from a remote peer", func(r *req) { r.token, r.peer, r.headers = agentTok, remote, nil }, 200},
		{"wrong token on a good local request", func(r *req) { r.token = wrongTok }, 401},
		{"inference token on a good local request", func(r *req) { r.token = infTok }, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hr := newHarness(t)
			r := modePut(`{"mode":"pause"}`)
			tc.mut(&r)
			if rec := hr.do(r); rec.Code != tc.want {
				t.Fatalf("got %d want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// Everything else still needs a token from this PC, even for a request that
// would pass the mode barrier.
func TestTokenlessExceptionIsModeOnly(t *testing.T) {
	cfgBody := mustJSON(newFake().cfg)
	for _, c := range []struct{ method, path, body string }{
		{"PUT", "/api/v1/config", cfgBody},
		{"PUT", "/api/v1/applications", `{"applications":[]}`},
		{"POST", "/api/v1/drain", `{}`},
		{"POST", "/api/v1/reload", `{}`},
		{"POST", "/api/v1/service/restart", `{}`},
		{"POST", "/api/v1/signin/codes", `{}`},
		{"POST", "/api/v1/agent/report", `{"session_id":1}`},
	} {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			hr := newHarness(t)
			r := modePut(c.body)
			r.method, r.path = c.method, c.path
			rec := hr.do(r)
			if rec.Code != 401 {
				t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
			}
			if len(hr.be.updates)+len(hr.be.drains)+len(hr.be.reloads)+len(hr.be.reports) != 0 {
				t.Fatal("a write reached the backend")
			}
		})
	}
}

// No CORS: a cross-origin page cannot pass a preflight, so it can never send
// the JSON content type the tokenless route requires.
func TestNoCORSPreflight(t *testing.T) {
	hr := newHarness(t)
	rec := hr.do(req{method: "OPTIONS", path: "/api/v1/mode", headers: map[string]string{
		"Origin": "https://evil.example", "Access-Control-Request-Method": "PUT", "Access-Control-Request-Headers": "content-type"}})
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("preflight: %d", rec.Code)
	}
	for k := range rec.Header() {
		if strings.HasPrefix(k, "Access-Control-") {
			t.Errorf("CORS header %s granted", k)
		}
	}
	allow := rec.Header().Get("Allow")
	if strings.Contains(allow, "OPTIONS") {
		t.Errorf("OPTIONS allowed: %s", allow)
	}
	// A real tokenless mode change still reports no CORS headers either.
	rec = hr.do(modePut(`{"mode":"pause"}`))
	for k := range rec.Header() {
		if strings.HasPrefix(k, "Access-Control-") {
			t.Errorf("CORS header %s on success", k)
		}
	}
}

func TestPrincipalNames(t *testing.T) {
	for p, want := range map[Principal]string{PrincipalNone: "local", PrincipalManagement: "management", PrincipalAgent: "agent", PrincipalSession: "session", PrincipalInvalid: "unknown"} {
		if p.String() != want {
			t.Errorf("%d: %s", p, p.String())
		}
	}
}
