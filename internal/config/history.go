package config

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"time"
)

const HistoryLimit = 20

var ErrHistoryNotFound = errors.New("configuration version not found")
var ErrHistoryConflict = errors.New("configuration changed since preview")
var ErrHistoryPrimaryUnusable = errors.New("saved configuration file is missing or invalid")

// HistoryEntry contains only values safe to show to a reader.
type HistoryEntry struct {
	ID        string          `json:"id"`
	Time      time.Time       `json:"time"`
	Actor     string          `json:"actor"`
	RestoreOf string          `json:"restore_of,omitempty"`
	Changes   []HistoryChange `json:"changes"`
}
type historyVersion struct {
	Entry  HistoryEntry    `json:"entry"`
	Config json.RawMessage `json:"config"`
}
type historyFile struct {
	Versions []historyVersion `json:"versions"`
	Pending  *historyVersion  `json:"pending,omitempty"`
}

func historyID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func (s *Store) historyPath() string { return s.path + ".history" }
func (s *Store) readHistory() (historyFile, error) {
	var h historyFile
	b, err := os.ReadFile(s.historyPath())
	if errors.Is(err, os.ErrNotExist) {
		return h, nil
	}
	if err != nil {
		return h, err
	}
	if err = json.Unmarshal(b, &h); err != nil {
		return h, fmt.Errorf("read config history: %w", err)
	}
	// The pending entry is committed only if its primary file was installed.
	if h.Pending != nil {
		active, e := os.ReadFile(s.path)
		var a, b bytes.Buffer
		if e == nil && json.Compact(&a, active) == nil && json.Compact(&b, h.Pending.Config) == nil && bytes.Equal(a.Bytes(), b.Bytes()) {
			h.Versions = append(h.Versions, *h.Pending)
		}
		h.Pending = nil
	}
	if len(h.Versions) > HistoryLimit {
		h.Versions = h.Versions[len(h.Versions)-HistoryLimit:]
	}
	return h, nil
}
func (s *Store) writeHistory(h historyFile) error {
	b, e := json.Marshal(h)
	if e != nil {
		return e
	}
	return WriteFileAtomic(s.historyPath(), b, 0600)
}

// SaveVersion writes a recoverable journal before replacing the primary.
// The journal is reconciled against the primary after a crash, so a failed
// primary replacement never turns into a successful history entry.
func (s *Store) SaveVersion(old, next Config, actor string, at time.Time, restoreOf string) error {
	if err := Validate(next); err != nil {
		return err
	}
	b, err := Marshal(next)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h, err := s.readHistory()
	if err != nil {
		return err
	}
	if len(h.Versions) == 0 {
		id, e := historyID()
		if e != nil {
			return e
		}
		ob, e := Marshal(old)
		if e != nil {
			return e
		}
		h.Versions = append(h.Versions, historyVersion{Entry: HistoryEntry{ID: id, Time: at, Actor: "initial", Changes: []HistoryChange{}}, Config: ob})
	}
	id, err := historyID()
	if err != nil {
		return err
	}
	h.Pending = &historyVersion{Entry: HistoryEntry{ID: id, Time: at, Actor: actor, RestoreOf: restoreOf, Changes: HistoryDiff(old, next)}, Config: b}
	if err = s.writeHistory(h); err != nil {
		return err
	}
	if err = WriteFileAtomic(s.path, b, 0600); err != nil {
		return err
	}
	// A pending record is sufficient for recovery if either following write fails.
	h.Versions = append(h.Versions, *h.Pending)
	h.Pending = nil
	if len(h.Versions) > HistoryLimit {
		h.Versions = h.Versions[len(h.Versions)-HistoryLimit:]
	}
	_ = s.writeHistory(h)
	_ = WriteFileAtomic(s.lastGood, b, 0600)
	return nil
}
func (s *Store) History() ([]HistoryEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, e := s.readHistory()
	if e != nil {
		return nil, e
	}
	out := make([]HistoryEntry, 0, len(h.Versions))
	for i := len(h.Versions) - 1; i >= 0; i-- {
		out = append(out, h.Versions[i].Entry)
	}
	return out, nil
}
func (s *Store) HistoryConfig(id string) (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, e := s.readHistory()
	if e != nil {
		return Config{}, e
	}
	for _, v := range h.Versions {
		if v.Entry.ID == id {
			return Parse(v.Config)
		}
	}
	return Config{}, ErrHistoryNotFound
}

// VerifyCurrent refuses restore if the primary file was changed externally,
// removed, or became invalid since it was loaded. Both sides are compared in
// normalized form, so a config that loading would adjust is not mistaken for
// a concurrent change.
func (s *Store) VerifyCurrent(current Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path)
	if err != nil {
		return ErrHistoryPrimaryUnusable
	}
	c, err := Parse(b)
	if err != nil {
		return ErrHistoryPrimaryUnusable
	}
	cb, err := Marshal(current)
	if err != nil {
		return err
	}
	want, err := Parse(cb)
	if err != nil || !reflect.DeepEqual(c, want) {
		return ErrHistoryConflict
	}
	return nil
}
