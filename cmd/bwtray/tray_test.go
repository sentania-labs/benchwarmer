package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/api"
	"github.com/sentania-labs/benchwarmer/internal/policy"
	"github.com/sentania-labs/benchwarmer/internal/state"
)

func TestIconIsValidICO(t *testing.T) {
	for _, c := range []state.Condition{state.Available, state.Yielding, state.Unavailable} {
		ico := iconFor(c, true)
		if binary.LittleEndian.Uint16(ico[2:]) != 1 || binary.LittleEndian.Uint16(ico[4:]) != 1 {
			t.Fatal("bad ICO header")
		}
		off := binary.LittleEndian.Uint32(ico[18:])
		if _, err := png.Decode(bytes.NewReader(ico[off:])); err != nil {
			t.Fatalf("%s: embedded PNG invalid: %v", c, err)
		}
	}
}

func TestTooltip(t *testing.T) {
	if got := tooltip(api.Status{}, errors.New("x")); !strings.Contains(got, "not reachable") {
		t.Fatal(got)
	}
	u := time.Now().Add(time.Hour)
	got := tooltip(api.Status{Summary: "Unavailable: paused", Mode: api.ModeStatus{Mode: policy.ModePause, Until: &u}}, nil)
	if !strings.Contains(got, "Paused until") || len(got) > 127 {
		t.Fatal(got)
	}
	long := tooltip(api.Status{Summary: strings.Repeat("x", 300)}, nil)
	if len(long) > 127 {
		t.Fatalf("tooltip too long: %d", len(long))
	}
}

func TestClientSendsAgentTokenAndMode(t *testing.T) {
	dir := t.TempDir()
	tf := filepath.Join(dir, "agent.token")
	if err := os.WriteFile(tf, []byte("agent-tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b := new(bytes.Buffer)
		_, _ = b.ReadFrom(r.Body)
		gotBody = b.String()
		if r.URL.Path == "/api/v1/status" {
			_, _ = w.Write([]byte(`{"condition":"Available","summary":"Available: model ready"}`))
		}
	}))
	defer srv.Close()
	c := newClient(srv.URL, tf)
	if err := c.setMode(policy.ModePause, "1h"); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer agent-tok" || !strings.Contains(gotBody, `"mode":"pause"`) || !strings.Contains(gotBody, `"set_by":"tray"`) {
		t.Fatalf("auth=%q body=%s", gotAuth, gotBody)
	}
	s, err := c.status()
	if err != nil || s.Condition != state.Available {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestReportPostsSessionFacts(t *testing.T) {
	var path, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		b := new(bytes.Buffer)
		_, _ = b.ReadFrom(r.Body)
		body = b.String()
	}))
	defer srv.Close()
	c := newClient(srv.URL, filepath.Join(t.TempDir(), "missing.token"))
	if err := c.report(api.AgentReport{SessionID: 1, ForegroundName: "eldenring.exe", Fullscreen: true, IdleSeconds: 2}); err != nil {
		t.Fatal(err)
	}
	if path != "/api/v1/agent/report" || !strings.Contains(body, `"foreground_name":"eldenring.exe"`) || !strings.Contains(body, `"fullscreen":true`) {
		t.Fatalf("%s %s", path, body)
	}
}
