package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/api"
	"github.com/sentania-labs/benchwarmer/internal/state"
)

// upstream imitates llama-server: streaming SSE and plain completions.
func upstream(t *testing.T, tokens int, delay time.Duration) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			http.Error(w, "credential leaked upstream", http.StatusTeapot)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"stream":true`)) {
			fmt.Fprint(w, `{"choices":[{"message":{"content":"hi"}}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < tokens; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(delay):
			}
			fmt.Fprintf(w, "data: {\"n\":%d}\n\n", i)
			fl.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(s.Close)
	return s
}

type obs struct {
	mu       sync.Mutex
	outcomes []string
	rejected int
}

func (o *obs) RequestFinished(_ string, _ time.Duration, outcome string) {
	o.mu.Lock()
	o.outcomes = append(o.outcomes, outcome)
	o.mu.Unlock()
}
func (o *obs) RequestRejected() { o.mu.Lock(); o.rejected++; o.mu.Unlock() }
func (o *obs) get() ([]string, int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.outcomes...), o.rejected
}

type rig struct {
	gate  *Gate
	srv   *httptest.Server
	obs   *obs
	logs  *bytes.Buffer
	logMu sync.Mutex
	max   atomic.Int64
}

func (r *rig) logText() string { r.logMu.Lock(); defer r.logMu.Unlock(); return r.logs.String() }

func newRig(t *testing.T, up *httptest.Server) *rig {
	t.Helper()
	r := &rig{gate: NewGate(nil), obs: &obs{}, logs: &bytes.Buffer{}}
	log := slog.New(slog.NewTextHandler(&lockedWriter{w: r.logs, mu: &r.logMu}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := New(Options{
		Gate:        r.gate,
		MaxDuration: func() time.Duration { return time.Duration(r.max.Load()) },
		Token:       func() string { return "inference-secret" },
		Condition:   func() state.Condition { return state.Available },
		Observer:    r.obs,
		Log:         log,
	})
	r.srv = httptest.NewServer(h)
	t.Cleanup(r.srv.Close)
	if up != nil {
		u, _ := url.Parse(up.URL)
		r.gate.Open(u)
	}
	return r
}

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func post(url, body string) (*http.Response, error) {
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}

func TestPlainCompletionPassesThrough(t *testing.T) {
	r := newRig(t, upstream(t, 1, 0))
	resp, err := post(r.srv.URL, `{"messages":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "hi") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if resp.Header.Get(HeaderRequestID) == "" || resp.Header.Get(HeaderCondition) != "Available" {
		t.Fatalf("headers: %v", resp.Header)
	}
}

func TestStreamingCompletes(t *testing.T) {
	r := newRig(t, upstream(t, 20, 5*time.Millisecond))
	resp, err := post(r.srv.URL, `{"stream":true}`)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	chunks, done := readSSE(resp.Body)
	if chunks != 20 || !done {
		t.Fatalf("chunks=%d done=%v", chunks, done)
	}
	waitIdle(t, r.gate)
}

func readSSE(r io.Reader) (chunks int, done bool) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		switch l := sc.Text(); {
		case l == "data: [DONE]":
			done = true
		case strings.HasPrefix(l, "data: "):
			chunks++
		}
	}
	return
}

func waitIdle(t *testing.T, g *Gate) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := g.Active(); n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("requests still active")
}

func TestClosedGateReturnsMachineReadable503(t *testing.T) {
	r := newRig(t, upstream(t, 1, 0))
	retry := time.Now().Add(3 * time.Minute)
	r.gate.Close(Closed{Condition: state.Yielding, Reason: "Game eldenring.exe is running", RetryAfter: retry})
	resp, err := post(r.srv.URL, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var e api.Error
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatal(err)
	}
	if e.Code != "worker_unavailable" || e.Condition != state.Yielding || e.RetryAfter == nil || e.Reason == "" {
		t.Fatalf("body %+v", e)
	}
	if resp.Header.Get("Retry-After") == "" || resp.Header.Get(HeaderCondition) != "Yielding" {
		t.Fatalf("headers %v", resp.Header)
	}
	if _, rej := r.obs.get(); rej != 1 {
		t.Fatalf("rejected %d", rej)
	}
}

// No request may be admitted once Close has returned, even under a flood.
func TestNoAdmissionAfterClose(t *testing.T) {
	r := newRig(t, upstream(t, 1, 0))
	var wg sync.WaitGroup
	var afterClose atomic.Bool
	var admittedAfter atomic.Int64
	stop := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				closedBefore := afterClose.Load()
				tk, _, _, ok := r.gate.admit(t.Context())
				if ok {
					if closedBefore {
						admittedAfter.Add(1)
					}
					r.gate.release(tk)
				}
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	r.gate.Close(Closed{Condition: state.Yielding})
	afterClose.Store(true)
	time.Sleep(20 * time.Millisecond)
	close(stop)
	wg.Wait()
	if n := admittedAfter.Load(); n != 0 {
		t.Fatalf("%d requests admitted after Close returned", n)
	}
}

func TestForceCloseCutsStream(t *testing.T) {
	r := newRig(t, upstream(t, 1000, 5*time.Millisecond))
	resp, err := post(r.srv.URL, `{"stream":true}`)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	res := make(chan [2]int, 1)
	go func() {
		c, d := readSSE(resp.Body)
		done := 0
		if d {
			done = 1
		}
		res <- [2]int{c, done}
	}()
	time.Sleep(50 * time.Millisecond)
	r.gate.Close(Closed{Condition: state.Yielding})
	if ids := r.gate.ForceCloseAll(); len(ids) != 1 {
		t.Fatalf("force closed %v", ids)
	}
	select {
	case got := <-res:
		if got[1] == 1 || got[0] == 0 || got[0] >= 1000 {
			t.Fatalf("stream should end abruptly mid-way: chunks=%d done=%d", got[0], got[1])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client never saw the cut")
	}
	waitIdle(t, r.gate)
	outs, _ := r.obs.get()
	if len(outs) != 1 || outs[0] != "force_closed" {
		t.Fatalf("outcomes %v", outs)
	}
}

func TestMaxDurationCutsRequest(t *testing.T) {
	r := newRig(t, upstream(t, 1000, 5*time.Millisecond))
	r.max.Store(int64(60 * time.Millisecond))
	resp, err := post(r.srv.URL, `{"stream":true}`)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, done := readSSE(resp.Body)
	if done {
		t.Fatal("request should have been cut")
	}
	waitIdle(t, r.gate)
	outs, _ := r.obs.get()
	if len(outs) != 1 || outs[0] != "timeout" {
		t.Fatalf("outcomes %v", outs)
	}
}

func TestNonLoopbackNeedsInferenceToken(t *testing.T) {
	r := newRig(t, upstream(t, 1, 0))
	h := r.srv.Config.Handler
	for _, tc := range []struct {
		auth string
		want int
	}{{"", 401}, {"Bearer wrong", 401}, {"Bearer inference-secret", 200}} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		req.RemoteAddr = "192.168.1.50:5555"
		if tc.auth != "" {
			req.Header.Set("Authorization", tc.auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("auth %q: got %d want %d (%s)", tc.auth, rec.Code, tc.want, rec.Body.String())
		}
	}
}

func TestCredentialNotForwardedAndNothingSensitiveLogged(t *testing.T) {
	r := newRig(t, upstream(t, 1, 0))
	req, _ := http.NewRequest(http.MethodPost, r.srv.URL+"/v1/chat/completions?key=querysecret", strings.NewReader(`{"messages":[{"content":"my private prompt"}]}`))
	req.Header.Set("Authorization", "Bearer inference-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("upstream saw a credential or failed: %d", resp.StatusCode)
	}
	waitIdle(t, r.gate)
	logs := r.logText()
	for _, secret := range []string{"inference-secret", "my private prompt", "querysecret"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("log contains %q:\n%s", secret, logs)
		}
	}
	if !strings.Contains(logs, "request_id") {
		t.Fatalf("expected metadata logging, got:\n%s", logs)
	}
}

func TestOnlyV1Served(t *testing.T) {
	r := newRig(t, upstream(t, 1, 0))
	resp, err := http.Get(r.srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

func TestLoopbackWithoutTokenRejectsForeignHost(t *testing.T) {
	r := newRig(t, upstream(t, 1, 0))
	h := r.srv.Config.Handler
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	req.RemoteAddr = "127.0.0.1:5555"
	req.Host = "evil.example.com:8480" // DNS-rebound page
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("foreign Host on loopback without token: %d", rec.Code)
	}
}
