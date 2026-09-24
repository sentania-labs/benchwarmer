package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sentania-labs/benchwarmer/internal/api"
	"github.com/sentania-labs/benchwarmer/internal/config"
)

func TestRegisterSignIn(t *testing.T) {
	var got api.SignInCode
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/signin/codes" || r.Header.Get("Authorization") != "Bearer mgmt" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"a bearer token is required","code":"unauthorized"}`))
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"expires_at":"2026-09-24T16:01:00-05:00"}`))
	}))
	defer srv.Close()
	code := strings.Repeat("a", 43)
	if err := registerSignIn(srv.URL, "mgmt", code); err != nil || got.Code != code {
		t.Fatalf("register: %v %+v", err, got)
	}
	if err := registerSignIn(srv.URL, "wrong", code); err == nil || !strings.Contains(err.Error(), "bearer token is required") {
		t.Fatalf("refusal not explained: %v", err)
	}
}

func TestManagementEndpointFromConfig(t *testing.T) {
	dir := t.TempDir()
	base, tf := managementEndpoint(dir)
	if base != "http://127.0.0.1:8481" || tf != filepath.Join(dir, "secrets", "management.token") {
		t.Fatalf("defaults: %s %s", base, tf)
	}
	c := config.Default()
	c.Listen.Management = "0.0.0.0:9000"
	b, _ := config.Marshal(c)
	_ = os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600)
	if base, _ := managementEndpoint(dir); base != "http://127.0.0.1:9000" {
		t.Fatalf("configured: %s", base)
	}
}
