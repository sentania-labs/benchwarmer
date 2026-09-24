package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

const code = "c0de-c0de-c0de-c0de-c0de-c0de-c0de-c0de"

func TestSignInCodeFlow(t *testing.T) {
	hr := newHarness(t)
	body := `{"code":"` + code + `"}`
	// Registering needs the management token, even from this PC.
	for _, tok := range []string{"", agentTok, infTok} {
		if rec := hr.do(req{method: "POST", path: "/api/v1/signin/codes", body: body, token: tok}); rec.Code == http.StatusOK {
			t.Fatalf("registered with token %q", tok)
		}
	}
	if rec := hr.do(req{method: "POST", path: "/api/v1/signin/codes", body: `{"code":"short"}`, token: mgmtTok}); rec.Code != http.StatusBadRequest {
		t.Fatalf("short code: %d", rec.Code)
	}
	if rec := hr.do(req{method: "POST", path: "/api/v1/signin/codes", body: body, token: mgmtTok}); rec.Code != http.StatusOK {
		t.Fatalf("register: %d %s", rec.Code, rec.Body)
	}
	// Redeemed only from this PC, addressed as loopback.
	if rec := hr.do(req{method: "POST", path: "/api/v1/signin/redeem", body: body, peer: remote}); rec.Code != http.StatusForbidden {
		t.Fatalf("remote redeem: %d", rec.Code)
	}
	if rec := hr.do(req{method: "POST", path: "/api/v1/signin/redeem", body: body, host: "attacker.example:8481"}); rec.Code != http.StatusForbidden {
		t.Fatalf("rebinding redeem: %d", rec.Code)
	}
	rec := hr.do(req{method: "POST", path: "/api/v1/signin/redeem", body: body})
	var got SignInToken
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.Token != mgmtTok {
		t.Fatalf("redeem: %d %s", rec.Code, rec.Body)
	}
	// Single use.
	if rec := hr.do(req{method: "POST", path: "/api/v1/signin/redeem", body: body}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("second redeem: %d", rec.Code)
	}
}

func TestSignInCodeExpiresAndLimits(t *testing.T) {
	now := time.Date(2026, 9, 24, 16, 0, 0, 0, time.Local)
	s := NewSignIn("tok", func() time.Time { return now })
	if _, err := s.Register(code); err != nil {
		t.Fatal(err)
	}
	now = now.Add(SignInCodeTTL)
	if _, ok := s.Redeem(code); ok {
		t.Fatal("expired code redeemed")
	}
	for i := 0; i < maxPendingCodes; i++ {
		if _, err := s.Register(strings.Repeat("a", 32+i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Register(strings.Repeat("b", 40)); err == nil {
		t.Fatal("unbounded pending codes")
	}
	// Guessing is cut off after repeated failures, even for a real code.
	for i := 0; i < maxRedeemFailure; i++ {
		s.Redeem(strings.Repeat("x", 32))
	}
	if _, ok := s.Redeem(strings.Repeat("a", 32)); ok {
		t.Fatal("redemption not paused after repeated failures")
	}
}

func TestSetupEndpoint(t *testing.T) {
	hr := newHarness(t)
	rec := hr.do(req{method: "GET", path: "/api/v1/setup"})
	var got SetupInfo
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || len(got.Models) != 1 {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body)
	}
	if rec := hr.do(req{method: "GET", path: "/api/v1/setup", peer: remote}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("remote without token: %d", rec.Code)
	}
}

func TestRestartEndpoint(t *testing.T) {
	hr := newHarness(t)
	if rec := hr.do(req{method: "POST", path: "/api/v1/service/restart"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("restart without token: %d", rec.Code)
	}
	if rec := hr.do(req{method: "POST", path: "/api/v1/service/restart", token: mgmtTok}); rec.Code != http.StatusNotFound {
		t.Fatalf("restart without a restarter: %d", rec.Code)
	}
}
