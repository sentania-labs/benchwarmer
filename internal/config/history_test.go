package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHistoryRetentionAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s := NewStore(path)
	initial, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	current := initial.Config
	for i := 0; i < 25; i++ {
		next := Clone(current)
		next.Runtime.ContextSize = 4096 + i
		next.Runtime.Args = []string{"--api-key", fmt.Sprintf("secret-%d", i)}
		if _, err = s.SaveVersion(current, next, "test", time.Unix(int64(i), 0), ""); err != nil {
			t.Fatal(err)
		}
		current = next
	}
	s = NewStore(path)
	loaded, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Config.Runtime.ContextSize != current.Runtime.ContextSize {
		t.Fatal("primary not preserved")
	}
	entries, err := s.History()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != HistoryLimit {
		t.Fatalf("got %d versions", len(entries))
	}
	b, _ := json.Marshal(entries)
	if strings.Contains(string(b), "secret-") {
		t.Fatal("history exposes secrets")
	}
	target, err := s.HistoryConfig(entries[19].ID)
	if err != nil {
		t.Fatal(err)
	}
	if target.Runtime.ContextSize != 4101 {
		t.Fatalf("wrong oldest retained: %d", target.Runtime.ContextSize)
	}
}

func TestHistoryRecoversInterruptedSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s := NewStore(path)
	r, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	next := Clone(r.Config)
	next.Runtime.ContextSize = 4096
	b, _ := Marshal(next)
	pending := historyVersion{Entry: HistoryEntry{ID: "pending", Time: time.Now(), Actor: "test", Changes: HistoryDiff(r.Config, next)}, Config: b}
	h := historyFile{Pending: &pending}
	if err = s.writeHistory(h); err != nil {
		t.Fatal(err)
	}
	entries, err := NewStore(path).History()
	if err != nil || len(entries) != 0 {
		t.Fatalf("uncommitted journal exposed: %v %v", entries, err)
	}
	if err = WriteFileAtomic(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	entries, err = NewStore(path).History()
	if err != nil || len(entries) != 1 || entries[0].ID != "pending" {
		t.Fatalf("committed journal lost: %v %v", entries, err)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryCorruptFileIsSetAsideAndSaveContinues(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "config.json"))
	r, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(s.historyPath(), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	next := Clone(r.Config)
	next.Runtime.ContextSize = 4096
	moved, err := s.SaveVersion(r.Config, next, "test", time.Now(), "")
	if err != nil {
		t.Fatalf("corrupt history blocked save: %v", err)
	}
	if moved == "" || !strings.HasPrefix(moved, s.historyPath()+".corrupt-") {
		t.Fatalf("unexpected moved path %q", moved)
	}
	if old, err := os.ReadFile(moved); err != nil || string(old) != "{" {
		t.Fatalf("corrupt file not preserved: %q %v", old, err)
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(b)
	if err != nil || c.Runtime.ContextSize != 4096 {
		t.Fatal("primary not saved")
	}
	entries, err := s.History()
	if err != nil || len(entries) != 2 || entries[0].Actor != "test" || entries[1].Actor != "initial" {
		t.Fatalf("fresh history not started: %+v %v", entries, err)
	}
	again := Clone(next)
	again.Runtime.ContextSize = 8192
	if moved, err = s.SaveVersion(next, again, "test", time.Now(), ""); err != nil || moved != "" {
		t.Fatalf("healthy history reported reset: %q %v", moved, err)
	}
}

func TestHistoryFailedPrimaryReplacementDoesNotCommitPending(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	s := NewStore(path)
	r, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(path, path+".backup"); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	next := Clone(r.Config)
	next.Runtime.ContextSize = 4096
	if _, err = s.SaveVersion(r.Config, next, "test", time.Now(), ""); err == nil {
		t.Fatal("expected failed primary replacement")
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(path+".backup", path); err != nil {
		t.Fatal(err)
	}
	entries, err := NewStore(path).History()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Actor != "initial" {
		t.Fatalf("failed save committed: %+v", entries)
	}
	c, err := s.HistoryConfig(entries[0].ID)
	if err != nil || c.Runtime.ContextSize != r.Config.Runtime.ContextSize {
		t.Fatal("old version not preserved")
	}
}

func TestHistoryRejectsInvalidSavedVersion(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "config.json"))
	r, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	h := historyFile{Versions: []historyVersion{{Entry: HistoryEntry{ID: "bad"}, Config: json.RawMessage(`{"schema_version":1}`)}}}
	if err = s.writeHistory(h); err != nil {
		t.Fatal(err)
	}
	if _, err = s.HistoryConfig("bad"); err == nil {
		t.Fatal("invalid saved config accepted")
	}
	current, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(current)
	if err != nil || c.Runtime.ContextSize != r.Config.Runtime.ContextSize {
		t.Fatal("primary changed")
	}
}

func TestVerifyCurrentIgnoresNormalizationButReportsUnusablePrimary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s := NewStore(path)
	initial, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	next := Clone(initial.Config)
	next.Listen.InferenceTLS.SelfSignedHosts = nil
	next.Runtime.ContextSize++
	if _, err = s.SaveVersion(initial.Config, next, "test", time.Unix(1, 0), ""); err != nil {
		t.Fatal(err)
	}
	if err = s.VerifyCurrent(next); err != nil {
		t.Fatalf("unnormalized current config reported as changed: %v", err)
	}
	other := Clone(next)
	other.Runtime.ContextSize++
	if err = s.VerifyCurrent(other); !errors.Is(err, ErrHistoryConflict) {
		t.Fatalf("real difference: %v", err)
	}
	if err = os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.VerifyCurrent(next); !errors.Is(err, ErrHistoryPrimaryUnusable) {
		t.Fatalf("invalid primary: %v", err)
	}
}
