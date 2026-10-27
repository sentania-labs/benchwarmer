package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
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

// An upstream may respond before the client has finished uploading. The proxy
// must not drain or close the upload while its transport still forwards it.
func TestStreamingResponseDuringUpload(t *testing.T) {
	upload := make(chan string, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			t.Error(err)
			return
		}
		prefix := make([]byte, 1)
		if _, err := io.ReadFull(r.Body, prefix); err != nil {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		rest, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		upload <- string(prefix) + string(rest)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(up.Close)
	r := newRig(t, up)
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	timer := time.AfterFunc(3*time.Second, func() { _ = writer.CloseWithError(fmt.Errorf("upload timed out waiting for the response")) })
	defer timer.Stop()
	go func() { _, _ = io.WriteString(writer, "{") }()
	req, err := http.NewRequest(http.MethodPost, r.srv.URL+"/v1/chat/completions", reader)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("early streaming response did not arrive: %v", err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	first, err := br.ReadString('\n')
	if err != nil || first != "data: first\n" {
		t.Fatalf("first chunk = %q, error = %v", first, err)
	}
	if _, err := io.WriteString(writer, "}"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	rest, err := io.ReadAll(br)
	if err != nil || !strings.Contains(string(rest), "data: [DONE]") {
		t.Fatalf("stream was truncated: %q, error = %v", rest, err)
	}
	select {
	case got := <-upload:
		if got != "{}" {
			t.Fatalf("forwarded upload = %q", got)
		}
	default:
		t.Fatal("upstream did not finish reading the upload")
	}
}

// runtimeReply imitates llama-server's usage reporting.
func runtimeReply(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"stream":true`)) {
			// The prompt echoes JSON-looking text; escaped, it must not count.
			fmt.Fprint(w, `{"choices":[{"message":{"content":"said \"prompt_tokens\":999"}}],"model":"small-q4.gguf",`+
				`"usage":{"prompt_tokens":12,"completion_tokens":34,"total_tokens":46}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"model\":\"small-q4.gguf\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fl.Flush()
		time.Sleep(20 * time.Millisecond)
		// Long enough to push the head out of the tail window.
		for i := 0; i < 2000; i++ {
			fmt.Fprintf(w, "data: {\"model\":\"small-q4.gguf\",\"choices\":[{\"delta\":{\"content\":\"tok%d\"}}]}\n\n", i)
		}
		fmt.Fprint(w, "data: {\"model\":\"small-q4.gguf\",\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2001},\"timings\":{\"prompt_n\":1,\"predicted_n\":1999}}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(s.Close)
	return s
}

func logLine(t *testing.T, r *rig, msg string) string {
	t.Helper()
	var found string
	waitFor(t, func() bool {
		for _, l := range strings.Split(r.logText(), "\n") {
			if strings.Contains(l, "msg=\""+msg+"\"") {
				found = l
				return true
			}
		}
		return false
	})
	return found
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

func TestPlainRequestLogsOneLine(t *testing.T) {
	r := newRig(t, runtimeReply(t))
	req, _ := http.NewRequest(http.MethodPost, r.srv.URL+"/v1/chat/completions?key=hidden", strings.NewReader(`{"messages":[{"content":"top secret prompt"}]}`))
	req.Header.Set("Authorization", "Bearer inference-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	l := logLine(t, r, "inference request")
	for _, want := range []string{"peer=127.0.0.1", "method=POST", "path=/v1/chat/completions", "status=200", "outcome=ok",
		"streamed=false", "model=small-q4.gguf", "prompt_tokens=12", "completion_tokens=34", "ttft_ms=", "duration_ms="} {
		if !strings.Contains(l, want) {
			t.Errorf("log line lacks %q: %s", want, l)
		}
	}
	for _, leak := range []string{"top secret", "hidden", "inference-secret", "said"} {
		if strings.Contains(r.logText(), leak) {
			t.Errorf("log leaks %q:\n%s", leak, r.logText())
		}
	}
	if n := strings.Count(r.logText(), `msg="inference request"`); n != 1 {
		t.Errorf("want exactly one line, got %d", n)
	}
}

func TestStreamedRequestLogsUsageFromFinalChunk(t *testing.T) {
	r := newRig(t, runtimeReply(t))
	req, _ := http.NewRequest(http.MethodPost, r.srv.URL+"/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	req.Header.Set("Authorization", "Bearer inference-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	l := logLine(t, r, "inference request")
	for _, want := range []string{"streamed=true", "model=small-q4.gguf", "prompt_tokens=5", "completion_tokens=2001", "status=200"} {
		if !strings.Contains(l, want) {
			t.Errorf("log line lacks %q: %s", want, l)
		}
	}
	if strings.Contains(r.logText(), "tok1999") {
		t.Error("log leaks response text")
	}
}

func TestRejectedWhileYieldingIsLogged(t *testing.T) {
	r := newRig(t, nil)
	r.gate.Close(Closed{Condition: state.Yielding, Reason: "a game is running"})
	req, _ := http.NewRequest(http.MethodPost, r.srv.URL+"/v1/chat/completions", strings.NewReader(`{"messages":[{"content":"top secret prompt"}]}`))
	req.Header.Set("Authorization", "Bearer inference-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	l := logLine(t, r, "inference request rejected")
	for _, want := range []string{"peer=127.0.0.1", "status=503", "condition=Yielding", `reason="a game is running"`, "path=/v1/chat/completions"} {
		if !strings.Contains(l, want) {
			t.Errorf("log line lacks %q: %s", want, l)
		}
	}
	if strings.Contains(r.logText(), "top secret") {
		t.Error("log leaks prompt")
	}
}

func TestUsageAttrsPreferUsageOverTimings(t *testing.T) {
	cases := []struct {
		name, body string
		want       []any
	}{
		{"usage wins over timings", `{"usage":{"prompt_tokens":900,"completion_tokens":7},"timings":{"prompt_n":3,"predicted_n":6}}`,
			[]any{"prompt_tokens", 900, "completion_tokens", 7}},
		{"timings alone", `{"timings":{"prompt_n":3,"predicted_n":6}}`,
			[]any{"prompt_tokens", 3, "completion_tokens", 6}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := &usage{}
			u.observe([]byte(c.body))
			got := u.attrs()
			var counts []any
			for i := 0; i+1 < len(got); i += 2 {
				if k := got[i]; k == "prompt_tokens" || k == "completion_tokens" {
					counts = append(counts, k, got[i+1])
				}
			}
			if fmt.Sprint(counts) != fmt.Sprint(c.want) {
				t.Errorf("got %v, want %v", counts, c.want)
			}
		})
	}
}

func TestCompressedStreamLogsStreamedWithoutCounts(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		fmt.Fprint(zw, "data: {\"model\":\"small-q4.gguf\",\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":7}}\n\ndata: [DONE]\n\n")
		zw.Close()
	}))
	t.Cleanup(up.Close)
	r := newRig(t, up)
	req, _ := http.NewRequest(http.MethodPost, r.srv.URL+"/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	req.Header.Set("Authorization", "Bearer inference-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	l := logLine(t, r, "inference request")
	if !strings.Contains(l, "streamed=true") {
		t.Errorf("compressed SSE not logged as streamed: %s", l)
	}
	for _, no := range []string{"prompt_tokens", "completion_tokens", "model="} {
		if strings.Contains(l, no) {
			t.Errorf("counts read from compressed bytes (%s): %s", no, l)
		}
	}
}

func TestUnauthorizedRequestIsLoggedWithoutToken(t *testing.T) {
	r := newRig(t, upstream(t, 1, 0))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?key=hidden", strings.NewReader(`{}`))
	req.RemoteAddr = "192.168.1.50:5555"
	req.Header.Set("Authorization", "Bearer wrong-token-value")
	rec := httptest.NewRecorder()
	r.srv.Config.Handler.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("status %d", rec.Code)
	}
	l := logLine(t, r, "inference request rejected")
	for _, want := range []string{"peer=192.168.1.50", "method=POST", "path=/v1/chat/completions", "status=401", "outcome=unauthorized"} {
		if !strings.Contains(l, want) {
			t.Errorf("log line lacks %q: %s", want, l)
		}
	}
	for _, leak := range []string{"wrong-token-value", "inference-secret", "hidden"} {
		if strings.Contains(r.logText(), leak) {
			t.Errorf("log leaks %q:\n%s", leak, r.logText())
		}
	}
	if n := strings.Count(r.logText(), "inference request"); n != 1 {
		t.Errorf("want exactly one line, got %d:\n%s", n, r.logText())
	}
}

func TestNotFoundRequestIsLogged(t *testing.T) {
	r := newRig(t, upstream(t, 1, 0))
	resp, err := http.Get(r.srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	l := logLine(t, r, "inference request rejected")
	for _, want := range []string{"peer=127.0.0.1", "method=GET", "path=/health", "status=404", "outcome=not_found"} {
		if !strings.Contains(l, want) {
			t.Errorf("log line lacks %q: %s", want, l)
		}
	}
	if n := strings.Count(r.logText(), "inference request"); n != 1 {
		t.Errorf("want exactly one line, got %d:\n%s", n, r.logText())
	}
}

func TestCutBeforeHeadersLogsDashStatus(t *testing.T) {
	started := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	}))
	t.Cleanup(up.Close)
	r := newRig(t, up)
	errc := make(chan error, 1)
	go func() {
		resp, err := post(r.srv.URL, `{}`)
		if resp != nil {
			resp.Body.Close()
		}
		errc <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached the runtime")
	}
	r.gate.Close(Closed{Condition: state.Yielding})
	r.gate.ForceCloseAll()
	if err := <-errc; err == nil {
		t.Fatal("client should see the cut")
	}
	waitIdle(t, r.gate)
	l := logLine(t, r, "inference request cut")
	for _, want := range []string{"status=-", "outcome=force_closed"} {
		if !strings.Contains(l, want) {
			t.Errorf("log line lacks %q: %s", want, l)
		}
	}
}

func TestLoggedPathIsBounded(t *testing.T) {
	r := newRig(t, upstream(t, 1, 0))
	long := "/v1/" + strings.Repeat("a", 4000)
	req := httptest.NewRequest(http.MethodPost, long, strings.NewReader(`{}`))
	req.RemoteAddr = "192.168.1.50:5555"
	r.srv.Config.Handler.ServeHTTP(httptest.NewRecorder(), req)
	req = httptest.NewRequest(http.MethodGet, "/"+strings.Repeat("b", 4000), nil)
	r.srv.Config.Handler.ServeHTTP(httptest.NewRecorder(), req)
	logs := r.logText()
	if got := strings.Count(logs, "inference request rejected"); got != 2 {
		t.Fatalf("want two rejected lines, got %d", got)
	}
	if len(logs) > 2000 {
		t.Errorf("logged paths not bounded: %d bytes", len(logs))
	}
}
