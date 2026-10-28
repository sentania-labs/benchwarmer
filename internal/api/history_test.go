package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/config"
	"github.com/sentania-labs/benchwarmer/internal/secrets"
)

type historyBackend struct {
	*fakeBackend
	calls int
}

func (b *historyBackend) ConfigHistory() ([]config.HistoryEntry, error) {
	return []config.HistoryEntry{{ID: "v1", Time: time.Now(), Actor: "api", Changes: []config.HistoryChange{{Path: "runtime.args", Before: config.Redacted, After: config.Redacted}}}}, nil
}
func (b *historyBackend) PreviewConfigRestore(id string) (RestorePreview, error) {
	if id != "v1" {
		return RestorePreview{}, config.ErrHistoryNotFound
	}
	return RestorePreview{ID: id, Revision: "revision", Impact: config.Impact{}}, nil
}
func (b *historyBackend) RestoreConfig(_ context.Context, id, rev, actor string) (config.Impact, error) {
	b.calls++
	if rev != "revision" {
		return config.Impact{}, config.ErrHistoryConflict
	}
	return config.Impact{}, nil
}
func TestHistoryRoutesAuthorizeAndRejectStaleRestore(t *testing.T) {
	hr := newHarness(t)
	b := &historyBackend{fakeBackend: hr.be}
	hr.h = New(Options{Backend: b, Auth: NewAuthenticator(secrets.Tokens{Management: mgmtTok, Agent: agentTok, Inference: infTok})})
	list := hr.do(req{method: "GET", path: "/api/v1/config/history", token: mgmtTok})
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), secret) {
		t.Fatalf("history: %d %s", list.Code, list.Body.String())
	}
	p := hr.do(req{method: "GET", path: "/api/v1/config/restore-preview?id=v1", token: mgmtTok})
	var preview RestorePreview
	if p.Code != 200 || json.Unmarshal(p.Body.Bytes(), &preview) != nil || preview.Revision != "revision" {
		t.Fatalf("preview: %s", p.Body.String())
	}
	denied := hr.do(req{method: "POST", path: "/api/v1/config/restore", body: `{"id":"v1","revision":"revision"}`, json: true})
	if denied.Code != 401 || b.calls != 0 {
		t.Fatalf("unauthorized restore: %d", denied.Code)
	}
	stale := hr.do(req{method: "POST", path: "/api/v1/config/restore", body: `{"id":"v1","revision":"stale"}`, json: true, token: mgmtTok})
	if stale.Code != 409 {
		t.Fatalf("stale restore: %d", stale.Code)
	}
	ok := hr.do(req{method: "POST", path: "/api/v1/config/restore", body: `{"id":"v1","revision":"revision"}`, json: true, token: mgmtTok})
	if ok.Code != 200 || strings.Contains(ok.Body.String(), secret) {
		t.Fatalf("restore: %d %s", ok.Code, ok.Body.String())
	}
}
