package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/config"
	"github.com/sentania-labs/benchwarmer/internal/events"
	"github.com/sentania-labs/benchwarmer/internal/policy"
	"github.com/sentania-labs/benchwarmer/internal/secrets"
	"github.com/sentania-labs/benchwarmer/internal/state"
)

const (
	mgmtTok  = "management-token-AAAAAAAAAAAAAAAAAAAAAAAA"
	agentTok = "agent-token-BBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	infTok   = "inference-token-CCCCCCCCCCCCCCCCCCCCCCCC"
	wrongTok = "wrong-token-DDDDDDDDDDDDDDDDDDDDDDDDDDDD"
	secret   = "hunter2-super-secret-api-key"
)

type fakeBackend struct {
	mu        sync.Mutex
	cfg       config.Config
	source    string
	events    []events.Event
	modes     []ModeRequest
	drains    []string
	reloads   []string
	reports   []AgentReport
	updates   []config.Config
	actors    []string
	lastQuery EventQuery
	failWith  error
}

func newFake() *fakeBackend {
	c := config.Default()
	c.Runtime.Args = []string{"--api-key", secret, "--threads", "8"}
	f := &fakeBackend{cfg: c, source: "primary"}
	for i := 1; i <= 250; i++ {
		typ := events.StateChanged
		if i%5 == 0 {
			typ = events.Preempted
		}
		f.events = append(f.events, events.Event{ID: int64(i), Type: typ, Message: fmt.Sprint(i)})
	}
	return f
}

func (f *fakeBackend) Status() Status {
	return Status{State: state.Ready, Condition: state.Available, ConfigSource: f.source,
		GPU: GPUStatus{Confidence: policy.ConfidenceHigh}}
}

func (f *fakeBackend) SetMode(req ModeRequest) (ModeStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return ModeStatus{}, f.failWith
	}
	f.modes = append(f.modes, req)
	return ModeStatus{Mode: req.Mode, SetBy: req.SetBy}, nil
}

func (f *fakeBackend) RequestDrain(reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drains = append(f.drains, reason)
	return f.failWith
}

func (f *fakeBackend) RequestReload(reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloads = append(f.reloads, reason)
	return f.failWith
}

func (f *fakeBackend) Config() (config.Config, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return config.Clone(f.cfg), f.source
}

func (f *fakeBackend) UpdateConfig(_ context.Context, c config.Config, actor string) (config.Impact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := config.Validate(c); err != nil {
		return config.Impact{}, err
	}
	im := config.Classify(f.cfg, c)
	f.cfg = c
	f.updates = append(f.updates, c)
	f.actors = append(f.actors, actor)
	return im, nil
}

func (f *fakeBackend) AgentReport(r AgentReport) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, r)
}

// Events mimics the store: newest first, BeforeID, Types, Limit.
func (f *fakeBackend) Events(q EventQuery) ([]events.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastQuery = q
	out := []events.Event{}
	for i := len(f.events) - 1; i >= 0 && len(out) < q.Limit; i-- {
		e := f.events[i]
		if q.BeforeID > 0 && e.ID >= q.BeforeID {
			continue
		}
		if len(q.Types) > 0 {
			match := false
			for _, t := range q.Types {
				match = match || t == e.Type
			}
			if !match {
				continue
			}
		}
		out = append(out, e)
	}
	return out, nil
}

type harness struct {
	t    *testing.T
	be   *fakeBackend
	h    http.Handler
	logs *bytes.Buffer
}

func newHarness(t *testing.T) *harness {
	be := newFake()
	logs := &bytes.Buffer{}
	metrics := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "benchwarmer_up 1\n")
	})
	h := New(Options{
		Backend: be,
		Auth:    NewAuthenticator(secrets.Tokens{Management: mgmtTok, Agent: agentTok, Inference: infTok}),
		Metrics: metrics, Version: "test",
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		SignIn: NewSignIn(mgmtTok, nil),
		Setup: func() SetupInfo {
			return SetupInfo{DataDir: `C:\ProgramData\Benchwarmer`, Models: []ModelFile{{Name: "m.gguf", Path: `C:\ProgramData\Benchwarmer\models\m.gguf`}}}
		},
	})
	hr := &harness{t: t, be: be, h: h, logs: logs}
	t.Cleanup(hr.assertLogsClean)
	return hr
}

// assertLogsClean fails if any credential or secret reached the log.
func (hr *harness) assertLogsClean() {
	out := hr.logs.String()
	for _, s := range []string{mgmtTok, agentTok, infTok, wrongTok, secret, "Bearer", "Authorization"} {
		if strings.Contains(out, s) {
			hr.t.Errorf("log contains %q:\n%s", s, out)
		}
	}
}

type req struct {
	method, path, body, token string
	peer, host                string
}

func (hr *harness) do(r req) *httptest.ResponseRecorder {
	hr.t.Helper()
	var body io.Reader
	if r.body != "" {
		body = strings.NewReader(r.body)
	}
	hreq := httptest.NewRequest(r.method, r.path, body)
	hreq.RemoteAddr = "127.0.0.1:50000"
	if r.peer != "" {
		hreq.RemoteAddr = r.peer
	}
	hreq.Host = "127.0.0.1:8481"
	if r.host != "" {
		hreq.Host = r.host
	}
	if r.token != "" {
		hreq.Header.Set("Authorization", "Bearer "+r.token)
	}
	rec := httptest.NewRecorder()
	hr.h.ServeHTTP(rec, hreq)
	return rec
}

func decodeErr(t *testing.T, rec *httptest.ResponseRecorder) Error {
	t.Helper()
	var e Error
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Code == "" {
		t.Fatalf("error body is not an api.Error (%d): %s", rec.Code, rec.Body.String())
	}
	return e
}

const remote = "192.168.1.50:50000"

func TestAuthMatrix(t *testing.T) {
	hr := newHarness(t)
	cfgBody := mustJSON(config.Default())
	report := `{"session_id":1,"fullscreen":false,"idle_seconds":3,"locked":false}`
	// Expected status per credential, in this order: loopback with no token,
	// remote with no token, wrong token, management, agent, inference. The
	// token columns are checked from both loopback and remote peers.
	cases := []struct {
		method, path, body string
		want               [6]int
	}{
		// Status and health: loopback trust, or the agent token (tray).
		{"GET", "/api/v1/health", "", [6]int{200, 401, 401, 200, 200, 403}},
		{"GET", "/api/v1/status", "", [6]int{200, 401, 401, 200, 200, 403}},
		// Other reads: loopback trust or the management token only.
		{"GET", "/api/v1/config", "", [6]int{200, 401, 401, 200, 403, 403}},
		{"GET", "/api/v1/events", "", [6]int{200, 401, 401, 200, 403, 403}},
		{"GET", "/api/v1/applications", "", [6]int{200, 401, 401, 200, 403, 403}},
		{"GET", "/metrics", "", [6]int{200, 401, 401, 200, 403, 403}},
		// Mode and agent reports: agent or management token, never tokenless.
		{"PUT", "/api/v1/mode", `{"mode":"auto"}`, [6]int{401, 401, 401, 200, 200, 403}},
		{"POST", "/api/v1/agent/report", report, [6]int{401, 401, 401, 204, 204, 403}},
		// Other writes: management token only, whatever the source.
		{"PUT", "/api/v1/config", cfgBody, [6]int{401, 401, 401, 200, 403, 403}},
		{"PUT", "/api/v1/applications", `{"applications":[]}`, [6]int{401, 401, 401, 200, 403, 403}},
		{"POST", "/api/v1/drain", "", [6]int{401, 401, 401, 202, 403, 403}},
		{"POST", "/api/v1/reload", "", [6]int{401, 401, 401, 202, 403, 403}},
	}
	creds := []struct {
		name  string
		token string
		peers []string
	}{
		{"none", "", []string{""}},
		{"none", "", []string{remote}},
		{"wrong", wrongTok, []string{"", remote}},
		{"management", mgmtTok, []string{"", remote}},
		{"agent", agentTok, []string{"", remote}},
		{"inference", infTok, []string{"", remote}},
	}
	for _, c := range cases {
		for i, cr := range creds {
			for _, peer := range cr.peers {
				where := "loopback"
				if peer != "" {
					where = "remote"
				}
				t.Run(fmt.Sprintf("%s %s/%s/%s", c.method, c.path, where, cr.name), func(t *testing.T) {
					rec := hr.do(req{method: c.method, path: c.path, body: c.body, token: cr.token, peer: peer})
					if rec.Code != c.want[i] {
						t.Fatalf("got %d want %d: %s", rec.Code, c.want[i], rec.Body.String())
					}
					if rec.Code >= 400 {
						decodeErr(t, rec)
						if rec.Code == 401 && rec.Header().Get("WWW-Authenticate") == "" {
							t.Error("401 without WWW-Authenticate")
						}
						for _, s := range []string{mgmtTok, agentTok, infTok, wrongTok} {
							if strings.Contains(rec.Body.String(), s) {
								t.Error("error body echoes a token")
							}
						}
					}
					if rec.Header().Get("Cache-Control") != "no-store" {
						t.Error("missing Cache-Control: no-store")
					}
				})
			}
		}
	}
}

func TestLoopbackTrustDisabled(t *testing.T) {
	hr := newHarness(t)
	hr.be.cfg.Security.LoopbackTrust = false
	if rec := hr.do(req{method: "GET", path: "/api/v1/status"}); rec.Code != 401 {
		t.Fatalf("loopback read without trust: %d", rec.Code)
	}
	if rec := hr.do(req{method: "GET", path: "/api/v1/status", token: mgmtTok}); rec.Code != 200 {
		t.Fatalf("loopback read with token: %d", rec.Code)
	}
}

func TestLoopbackDeterminedByPeerOnly(t *testing.T) {
	hr := newHarness(t)
	hreq := httptest.NewRequest("GET", "/api/v1/status", nil)
	hreq.RemoteAddr = remote
	hreq.Header.Set("X-Forwarded-For", "127.0.0.1")
	hreq.Header.Set("X-Real-IP", "127.0.0.1")
	hreq.Host = "localhost:8481"
	rec := httptest.NewRecorder()
	hr.h.ServeHTTP(rec, hreq)
	if rec.Code != 401 {
		t.Fatalf("forwarded header was trusted: %d", rec.Code)
	}
	// IPv6 loopback and localhost Host are trusted.
	if rec := hr.do(req{method: "GET", path: "/api/v1/status", peer: "[::1]:50000", host: "localhost:8481"}); rec.Code != 200 {
		t.Fatalf("::1 read: %d", rec.Code)
	}
	if rec := hr.do(req{method: "GET", path: "/api/v1/status", host: "[::1]:8481"}); rec.Code != 200 {
		t.Fatalf("[::1] host: %d", rec.Code)
	}
}

func TestDNSRebindingHostRefused(t *testing.T) {
	hr := newHarness(t)
	rec := hr.do(req{method: "GET", path: "/api/v1/config", host: "evil.example:8481"})
	if rec.Code != 401 {
		t.Fatalf("rebinding host: %d", rec.Code)
	}
	if rec := hr.do(req{method: "GET", path: "/api/v1/config", host: "evil.example:8481", token: mgmtTok}); rec.Code != 200 {
		t.Fatalf("token should still work with any host: %d", rec.Code)
	}
}

func TestConfigRedactedAndRoundTrip(t *testing.T) {
	hr := newHarness(t)
	rec := hr.do(req{method: "GET", path: "/api/v1/config"})
	if rec.Code != 200 || strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("GET config leaked secret or failed (%d): %s", rec.Code, rec.Body.String())
	}
	var got ConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Config.Runtime.Args[1] != config.Redacted || got.Source != "primary" {
		t.Fatalf("args=%v source=%q", got.Config.Runtime.Args, got.Source)
	}

	// Change one field and PUT the redacted document back.
	got.Config.Runtime.ContextSize = 16384
	body, _ := json.Marshal(got.Config)
	rec = hr.do(req{method: "PUT", path: "/api/v1/config", body: string(body), token: mgmtTok})
	if rec.Code != 200 {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatal("PUT response leaked secret")
	}
	var put ConfigResponse
	json.Unmarshal(rec.Body.Bytes(), &put)
	if put.Impact == nil || !put.Impact.RuntimeReload {
		t.Fatalf("impact %+v", put.Impact)
	}
	stored := hr.be.updates[len(hr.be.updates)-1]
	if stored.Runtime.Args[1] != secret || stored.Runtime.ContextSize != 16384 {
		t.Fatalf("secret not restored: %v", stored.Runtime.Args)
	}
	if hr.be.actors[0] != "api@127.0.0.1" {
		t.Fatalf("actor %q", hr.be.actors[0])
	}
}

func TestConfigValidationErrorShape(t *testing.T) {
	hr := newHarness(t)
	c := config.Default()
	c.Runtime.Host = "0.0.0.0"
	c.Safety.GPUTempCriticalC = 200
	body, _ := json.Marshal(c)
	rec := hr.do(req{method: "PUT", path: "/api/v1/config", body: string(body), token: mgmtTok})
	if rec.Code != 400 {
		t.Fatalf("got %d", rec.Code)
	}
	e := decodeErr(t, rec)
	fields := map[string]bool{}
	for _, d := range e.Details {
		fields[d.Field] = true
	}
	if e.Code != CodeInvalidConfig || !fields["runtime.host"] || !fields["safety.gpu_temp_critical_c"] {
		t.Fatalf("error %+v", e)
	}
	if len(hr.be.updates) != 0 {
		t.Fatal("invalid config reached UpdateConfig")
	}
}

func TestConfigStrictDecode(t *testing.T) {
	hr := newHarness(t)
	b, _ := json.Marshal(config.Default())
	withTypo := strings.Replace(string(b), `"timezone"`, `"time_zone"`, 1)
	rec := hr.do(req{method: "PUT", path: "/api/v1/config", body: withTypo, token: mgmtTok})
	if rec.Code != 400 || decodeErr(t, rec).Code != CodeBadRequest || !strings.Contains(rec.Body.String(), "time_zone") {
		t.Fatalf("unknown field: %d %s", rec.Code, rec.Body.String())
	}
	rec = hr.do(req{method: "PUT", path: "/api/v1/config", body: string(b) + "{}", token: mgmtTok})
	if rec.Code != 400 {
		t.Fatalf("trailing data: %d", rec.Code)
	}
	rec = hr.do(req{method: "PUT", path: "/api/v1/config", body: `{"schema_version":`, token: mgmtTok})
	if rec.Code != 400 {
		t.Fatalf("truncated: %d", rec.Code)
	}
}

func TestBodyLimits(t *testing.T) {
	hr := newHarness(t)
	big := `{"mode":"pause","set_by":"` + strings.Repeat("x", maxSmallBody) + `"}`
	rec := hr.do(req{method: "PUT", path: "/api/v1/mode", body: big, token: mgmtTok})
	if rec.Code != 413 || decodeErr(t, rec).Code != CodeBodyTooLarge {
		t.Fatalf("mode body: %d %s", rec.Code, rec.Body.String())
	}
	// A config just over 16 KiB is fine; over 1 MiB is not.
	c := config.Default()
	for len(mustJSON(c)) < 20<<10 {
		c.Applications = append(c.Applications, config.AppRule{Name: "pad", Exe: "pad.exe", Class: config.ClassOrdinary})
	}
	if rec := hr.do(req{method: "PUT", path: "/api/v1/config", body: mustJSON(c), token: mgmtTok}); rec.Code != 200 {
		t.Fatalf("20 KiB config: %d %s", rec.Code, rec.Body.String())
	}
	huge := `{"applications":[` + strings.Repeat(`{"exe":"a.exe","class":"game"},`, (1<<20)/30) + `{"exe":"a.exe","class":"game"}]}`
	rec = hr.do(req{method: "PUT", path: "/api/v1/applications", body: huge, token: mgmtTok})
	if rec.Code != 413 {
		t.Fatalf("huge applications: %d", rec.Code)
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestModeParsing(t *testing.T) {
	cases := []struct {
		body string
		want int
	}{
		{`{"mode":"pause"}`, 200},
		{`{"mode":"pause","duration":"30m"}`, 200},
		{`{"mode":"pause","duration":"until_reboot"}`, 200},
		{`{"mode":"ai_priority","duration":"until_tomorrow"}`, 200},
		{`{"mode":"ai_priority","duration":"12h"}`, 200},
		{`{"mode":"ai_priority","duration":"13h"}`, 400}, // over modes.max_ai_priority
		{`{"mode":"pause","duration":"720h"}`, 200},
		{`{"mode":"pause","duration":"721h"}`, 400},
		{`{"mode":"auto"}`, 200},
		{`{"mode":"auto","duration":"1h"}`, 400},
		{`{"mode":"auto","duration":"until_reboot"}`, 400},
		{`{"mode":"pause","duration":"-5m"}`, 400},
		{`{"mode":"pause","duration":"0s"}`, 400},
		{`{"mode":"pause","duration":"soon"}`, 400},
		{`{"mode":"turbo"}`, 400},
		{`{"mode":""}`, 400},
		{`{"mode":"pause","duraton":"1h"}`, 400}, // strict decode catches typos
		{``, 400},
	}
	for _, c := range cases {
		hr := newHarness(t)
		rec := hr.do(req{method: "PUT", path: "/api/v1/mode", body: c.body, token: mgmtTok})
		if rec.Code != c.want {
			t.Errorf("%s: got %d want %d: %s", c.body, rec.Code, c.want, rec.Body.String())
			continue
		}
		if c.want == 400 {
			decodeErr(t, rec)
			if len(hr.be.modes) != 0 {
				t.Errorf("%s: invalid mode reached backend", c.body)
			}
		}
	}
	hr := newHarness(t)
	hr.do(req{method: "PUT", path: "/api/v1/mode", body: `{"mode":"pause","duration":"1h"}`, token: mgmtTok})
	if m := hr.be.modes[0]; m.SetBy != "api" || m.Duration != "1h" || m.Mode != policy.ModePause {
		t.Fatalf("passed %+v", m)
	}
}

func TestBackendErrors(t *testing.T) {
	hr := newHarness(t)
	hr.be.failWith = Rejectf("AI Priority is not allowed while the GPU is too hot")
	rec := hr.do(req{method: "PUT", path: "/api/v1/mode", body: `{"mode":"ai_priority"}`, token: mgmtTok})
	if rec.Code != 409 || decodeErr(t, rec).Code != CodeRejected {
		t.Fatalf("rejected: %d %s", rec.Code, rec.Body.String())
	}
	hr.be.failWith = errors.New("disk exploded at C:\\ProgramData")
	rec = hr.do(req{method: "POST", path: "/api/v1/reload", token: mgmtTok})
	if rec.Code != 500 || decodeErr(t, rec).Code != CodeInternal || strings.Contains(rec.Body.String(), "exploded") {
		t.Fatalf("internal: %d %s", rec.Code, rec.Body.String())
	}
}

func TestEventsPaging(t *testing.T) {
	hr := newHarness(t)
	get := func(q string) EventsResponse {
		t.Helper()
		rec := hr.do(req{method: "GET", path: "/api/v1/events" + q})
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", q, rec.Code, rec.Body.String())
		}
		var out EventsResponse
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	r := get("")
	if len(r.Events) != 100 || r.Events[0].ID != 250 || r.NextBeforeID != 151 {
		t.Fatalf("default page: n=%d first=%d next=%d", len(r.Events), r.Events[0].ID, r.NextBeforeID)
	}
	r = get("?limit=100&before_id=51")
	if len(r.Events) != 50 || r.NextBeforeID != 0 {
		t.Fatalf("last page: n=%d next=%d", len(r.Events), r.NextBeforeID)
	}
	r = get("?limit=5000")
	if hr.be.lastQuery.Limit != maxEventsLimit+1 || len(r.Events) != 250 || r.NextBeforeID != 0 {
		t.Fatalf("clamped: query=%d n=%d", hr.be.lastQuery.Limit, len(r.Events))
	}
	r = get("?type=preempted&limit=10")
	if len(r.Events) != 10 || r.Events[0].ID != 250 || r.Events[9].ID != 205 || r.NextBeforeID != 205 {
		t.Fatalf("filtered: n=%d next=%d", len(r.Events), r.NextBeforeID)
	}
	get("?type=preempted&type=state_changed,mode_changed")
	if len(hr.be.lastQuery.Types) != 3 {
		t.Fatalf("types %v", hr.be.lastQuery.Types)
	}
	r = get("?before_id=1")
	if r.Events == nil || len(r.Events) != 0 {
		t.Fatalf("empty page should be []: %+v", r)
	}
	for _, q := range []string{"?limit=0", "?limit=-1", "?limit=abc", "?before_id=0", "?before_id=x"} {
		if rec := hr.do(req{method: "GET", path: "/api/v1/events" + q}); rec.Code != 400 {
			t.Errorf("%s: %d", q, rec.Code)
		}
	}
}

func TestApplications(t *testing.T) {
	hr := newHarness(t)
	rec := hr.do(req{method: "GET", path: "/api/v1/applications"})
	var got ApplicationsBody
	json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != 200 || len(got.Applications) != len(config.DefaultApplications()) {
		t.Fatalf("GET: %d n=%d", rec.Code, len(got.Applications))
	}
	body := `{"applications":[{"name":"Elden Ring","exe":"eldenring.exe","class":"game"}]}`
	rec = hr.do(req{method: "PUT", path: "/api/v1/applications", body: body, token: mgmtTok})
	if rec.Code != 200 {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	stored := hr.be.updates[0]
	if len(stored.Applications) != 1 || stored.Applications[0].Exe != "eldenring.exe" || stored.Runtime.Args[1] != secret {
		t.Fatalf("stored %+v args=%v", stored.Applications, stored.Runtime.Args)
	}
	rec = hr.do(req{method: "PUT", path: "/api/v1/applications", body: `{"applications":[{"exe":"a.exe","class":"weird"}]}`, token: mgmtTok})
	if rec.Code != 400 || decodeErr(t, rec).Code != CodeInvalidConfig {
		t.Fatalf("invalid rule: %d %s", rec.Code, rec.Body.String())
	}
	rec = hr.do(req{method: "PUT", path: "/api/v1/applications", body: `{}`, token: mgmtTok})
	if rec.Code != 400 {
		t.Fatalf("missing applications: %d", rec.Code)
	}
}

func TestActionsAndAgent(t *testing.T) {
	hr := newHarness(t)
	rec := hr.do(req{method: "POST", path: "/api/v1/drain", body: `{"reason":"testing"}`, token: mgmtTok})
	var ar ActionResponse
	json.Unmarshal(rec.Body.Bytes(), &ar)
	if rec.Code != 202 || !ar.Accepted || ar.Action != "drain" || hr.be.drains[0] != "testing" {
		t.Fatalf("drain: %d %+v %v", rec.Code, ar, hr.be.drains)
	}
	if rec := hr.do(req{method: "POST", path: "/api/v1/reload", token: mgmtTok}); rec.Code != 202 || hr.be.reloads[0] == "" {
		t.Fatalf("reload: %d", rec.Code)
	}
	long := `{"reason":"` + strings.Repeat("r", maxReasonLen+1) + `"}`
	if rec := hr.do(req{method: "POST", path: "/api/v1/drain", body: long, token: mgmtTok}); rec.Code != 400 {
		t.Fatalf("long reason: %d", rec.Code)
	}
	// Agent reports tolerate unknown fields from a newer agent.
	body := `{"session_id":2,"foreground_name":"game.exe","fullscreen":true,"idle_seconds":0,"locked":false,"future_field":1}`
	if rec := hr.do(req{method: "POST", path: "/api/v1/agent/report", body: body, token: agentTok}); rec.Code != 204 {
		t.Fatalf("agent: %d %s", rec.Code, rec.Body.String())
	}
	if r := hr.be.reports[0]; r.SessionID != 2 || !r.Fullscreen || r.ForegroundName != "game.exe" {
		t.Fatalf("report %+v", r)
	}
}

func TestHealthAndStatus(t *testing.T) {
	hr := newHarness(t)
	rec := hr.do(req{method: "GET", path: "/api/v1/health"})
	var h Health
	json.Unmarshal(rec.Body.Bytes(), &h)
	if rec.Code != 200 || !h.OK || h.Version != "test" {
		t.Fatalf("health %d %+v", rec.Code, h)
	}
	hr.be.source = "last_good"
	json.Unmarshal(hr.do(req{method: "GET", path: "/api/v1/health"}).Body.Bytes(), &h)
	if h.OK || len(h.Problems) != 1 {
		t.Fatalf("degraded health %+v", h)
	}
	var s Status
	rec = hr.do(req{method: "GET", path: "/api/v1/status"})
	json.Unmarshal(rec.Body.Bytes(), &s)
	if s.State != state.Ready || rec.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("status %+v", s)
	}
}

func TestMetricsAndRouting(t *testing.T) {
	hr := newHarness(t)
	if rec := hr.do(req{method: "GET", path: "/metrics"}); rec.Code != 200 || !strings.Contains(rec.Body.String(), "benchwarmer_up 1") {
		t.Fatalf("metrics loopback: %d", rec.Code)
	}
	if rec := hr.do(req{method: "GET", path: "/metrics", peer: remote}); rec.Code != 401 {
		t.Fatalf("metrics remote without token: %d", rec.Code)
	}
	if rec := hr.do(req{method: "GET", path: "/metrics", peer: remote, token: mgmtTok}); rec.Code != 200 {
		t.Fatalf("metrics remote with token: %d", rec.Code)
	}
	hr.be.cfg.Metrics.Enabled = false
	if rec := hr.do(req{method: "GET", path: "/metrics"}); rec.Code != 404 {
		t.Fatalf("disabled metrics: %d", rec.Code)
	}
	rec := hr.do(req{method: "DELETE", path: "/api/v1/config", token: mgmtTok})
	if rec.Code != 405 || rec.Header().Get("Allow") != "GET, PUT" || decodeErr(t, rec).Code != CodeMethodNotAllowed {
		t.Fatalf("405: %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	if rec := hr.do(req{method: "GET", path: "/api/v1/nope"}); rec.Code != 404 || decodeErr(t, rec).Code != CodeNotFound {
		t.Fatalf("404: %d", rec.Code)
	}
}

func TestLogsCarryNoSecrets(t *testing.T) {
	hr := newHarness(t)
	c, _ := hr.be.Config()
	body := mustJSON(c) // contains the unredacted secret
	hr.do(req{method: "PUT", path: "/api/v1/config", body: body, token: mgmtTok})
	hr.do(req{method: "GET", path: "/api/v1/status", token: wrongTok, peer: remote})
	hr.do(req{method: "GET", path: "/api/v1/events?type=" + secret})
	hr.be.failWith = errors.New("backend failure")
	hr.do(req{method: "POST", path: "/api/v1/drain", body: `{"reason":"` + secret + `"}`, token: agentTok})
	hr.do(req{method: "POST", path: "/api/v1/drain", token: mgmtTok})
	if !strings.Contains(hr.logs.String(), "management request") {
		t.Fatal("expected request log lines")
	}
	// assertLogsClean runs at cleanup.
}

func TestWriteUnavailable(t *testing.T) {
	now := time.Date(2026, 9, 23, 14, 0, 0, 0, time.UTC)
	retry := now.Add(90*time.Second + 300*time.Millisecond)
	rec := httptest.NewRecorder()
	WriteUnavailable(rec, now, state.Yielding, "a game is running", &retry)
	e := decodeErr(t, rec)
	if rec.Code != 503 || e.Code != CodeWorkerUnavailable || e.Condition != state.Yielding || e.Reason != "a game is running" ||
		!e.RetryAfter.Equal(retry) || rec.Header().Get("Retry-After") != "91" || rec.Header().Get(HeaderCondition) != "Yielding" {
		t.Fatalf("503: %d %+v headers=%v", rec.Code, e, rec.Header())
	}
}
