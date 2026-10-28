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
	r := newRigWith(t, nil, func(d *Deps) { d.ConfigStore = store; d.CheckModelFile = func(string) error { return nil } })
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
	_, digest, _ := strings.Cut(preview.Revision, ":")
	if _, err = r.c.RestoreConfig(context.Background(), preview.ID, ":"+digest, "test"); !errors.Is(err, config.ErrHistoryConflict) {
		t.Fatalf("empty revision token accepted: %v", err)
	}
	if current, _ := r.c.Config(); current.Runtime.ContextSize != other.Runtime.ContextSize {
		t.Fatal("empty token overwrote newer config")
	}
	if _, err = r.c.RestoreConfig(context.Background(), preview.ID, preview.Revision, "test"); !errors.Is(err, config.ErrHistoryConflict) {
		t.Fatalf("stale preview: %v", err)
	}
	preview, err = r.c.PreviewConfigRestore(preview.ID)
	if err != nil {
		t.Fatal(err)
	}
	r.c.d.CheckModelFile = func(string) error { return errors.New("no file exists at this path") }
	if _, err = r.c.RestoreConfig(context.Background(), preview.ID, preview.Revision, "test"); err == nil {
		t.Fatal("missing model accepted")
	}
	r.c.d.CheckModelFile = func(string) error { return nil }
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
	if _, err = r.c.RestoreConfig(context.Background(), preview.ID, preview.Revision, "test"); !errors.Is(err, config.ErrHistoryPrimaryUnusable) {
		t.Fatalf("missing primary accepted: %v", err)
	}
}

func TestUnreadableHistoryDoesNotBlockSaveAndWarns(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	r := newRigWith(t, nil, func(d *Deps) { d.ConfigStore = store; d.CheckModelFile = func(string) error { return nil } })
	b, _ := config.Marshal(r.cfg)
	if err := config.WriteFileAtomic(store.Path(), b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path()+".history", []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	next := config.Clone(r.cfg)
	next.Runtime.ContextSize = 4096
	if _, err := r.c.UpdateConfig(context.Background(), next, "test"); err != nil {
		t.Fatalf("save blocked by unreadable history: %v", err)
	}
	moved, err := filepath.Glob(store.Path() + ".history.corrupt-*")
	if err != nil || len(moved) != 1 {
		t.Fatalf("corrupt history not set aside: %v %v", moved, err)
	}
	var warned bool
	r.ev.mu.Lock()
	defer r.ev.mu.Unlock()
	for _, e := range r.ev.ev {
		if e.Data["history_reset"] == true && e.Data["moved_to"] == moved[0] && strings.Contains(e.Message, moved[0]) {
			warned = true
		}
	}
	if !warned {
		t.Fatal("missing history reset warning")
	}
}
