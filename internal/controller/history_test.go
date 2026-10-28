package controller

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sentania-labs/benchwarmer/internal/config"
	"github.com/sentania-labs/benchwarmer/internal/events"
)

func TestRestoreConflictValidationAndAudit(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	r := newRigWith(t, nil, func(d *Deps) { d.ConfigStore = store; d.FileExists = func(string) bool { return true } })
	b, _ := config.Marshal(r.cfg)
	if err := config.WriteFileAtomic(store.Path(), b, 0600); err != nil {
		t.Fatal(err)
	}
	first := config.Clone(r.cfg)
	first.Runtime.ContextSize = 4096
	first.Runtime.Args = []string{"--api-key", "test-secret-value"}
	if _, err := r.c.UpdateConfig(context.Background(), first, "test"); err != nil {
		t.Fatal(err)
	}
	versions, err := r.c.ConfigHistory()
	if err != nil {
		t.Fatal(err)
	}
	preview, err := r.c.PreviewConfigRestore(versions[len(versions)-1].ID)
	if err != nil {
		t.Fatal(err)
	}
	other := config.Clone(first)
	other.Runtime.ContextSize = 8192
	if _, err = r.c.UpdateConfig(context.Background(), other, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err = r.c.RestoreConfig(context.Background(), preview.ID, preview.Revision, "test"); !errors.Is(err, config.ErrHistoryConflict) {
		t.Fatalf("stale preview: %v", err)
	}
	preview, err = r.c.PreviewConfigRestore(preview.ID)
	if err != nil {
		t.Fatal(err)
	}
	r.c.d.FileExists = func(string) bool { return false }
	if _, err = r.c.RestoreConfig(context.Background(), preview.ID, preview.Revision, "test"); err == nil {
		t.Fatal("missing model accepted")
	}
	r.c.d.FileExists = func(string) bool { return true }
	if _, err = r.c.RestoreConfig(context.Background(), preview.ID, preview.Revision, "test"); err != nil {
		t.Fatal(err)
	}
	versions, err = r.c.ConfigHistory()
	if err != nil || versions[0].RestoreOf != preview.ID {
		t.Fatalf("missing restore audit: %v %v", versions, err)
	}
	if event, ok := r.ev.find(events.ConfigChanged); !ok || event.Data["restore_of"] != preview.ID {
		t.Fatal("missing restore event")
	} else {
		b, _ := json.Marshal(event)
		if strings.Contains(string(b), "test-secret-value") {
			t.Fatal("secret in event")
		}
		if event.Data["changes"] == nil {
			t.Fatal("missing event changes")
		}
	}
	preview, err = r.c.PreviewConfigRestore(preview.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(store.Path()); err != nil {
		t.Fatal(err)
	}
	if _, err = r.c.RestoreConfig(context.Background(), preview.ID, preview.Revision, "test"); !errors.Is(err, config.ErrHistoryConflict) {
		t.Fatalf("missing primary accepted: %v", err)
	}
}
