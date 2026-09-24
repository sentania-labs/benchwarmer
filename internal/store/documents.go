package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/policy"
)

// Document keys. Each is one small JSON row, rewritten whole.
const (
	keyMode    = "mode"
	keyTimers  = "timers"
	keyRuntime = "runtime"
)

// ModeState is the persisted manual mode, so a timed Pause AI or AI Priority
// survives a service restart and still expires on time.
type ModeState struct {
	Mode  policy.Mode `json:"mode"`
	Until *time.Time  `json:"until,omitempty"`
	SetBy string      `json:"set_by,omitempty"`
	SetAt time.Time   `json:"set_at,omitzero"`
	// UntilReboot marks a mode that ends at the next boot. BootTime is the
	// system boot time when it was set; the controller discards the mode on
	// startup when the current boot time differs.
	UntilReboot bool      `json:"until_reboot,omitempty"`
	BootTime    time.Time `json:"boot_time,omitzero"`
}

// Timers are the controller's anti-thrash, cooldown, and crash-backoff
// records (policy.TimerFacts plus backoff), persisted so a restart cannot be
// used to skip a cooldown or suppression.
type Timers struct {
	Preemptions       []time.Time `json:"preemptions,omitempty"`
	SuppressedAt      time.Time   `json:"suppressed_at,omitzero"`
	LastCompetingAt   time.Time   `json:"last_competing_at,omitzero"`
	LastCompetingRule string      `json:"last_competing_rule,omitempty"`
	CrashCount        int         `json:"crash_count,omitempty"`
	// BackoffAttempt is the exponent of the current crash backoff; reset
	// when the runtime stays up for recovery.crash_reset_after.
	BackoffAttempt int       `json:"backoff_attempt,omitempty"`
	LastCrashAt    time.Time `json:"last_crash_at,omitzero"`
}

// RuntimeMemo holds measurements from the last successful load, used before
// the runtime is loaded again (e.g. to infer external VRAM under degraded
// telemetry and to show expected load time).
type RuntimeMemo struct {
	FootprintMiB    int       `json:"footprint_mib,omitempty"`
	LastLoadSeconds float64   `json:"last_load_seconds,omitempty"`
	MeasuredAt      time.Time `json:"measured_at,omitzero"`
}

// ModeState returns the persisted mode; ok is false when none was saved.
func (s *Store) ModeState() (m ModeState, ok bool, err error) {
	ok, err = s.getDoc(keyMode, &m)
	return m, ok, err
}

// SaveModeState persists the mode.
func (s *Store) SaveModeState(m ModeState) error { return s.putDoc(keyMode, m) }

// Timers returns the persisted timers; ok is false when none were saved.
func (s *Store) Timers() (t Timers, ok bool, err error) {
	ok, err = s.getDoc(keyTimers, &t)
	return t, ok, err
}

// SaveTimers persists the timers.
func (s *Store) SaveTimers(t Timers) error { return s.putDoc(keyTimers, t) }

// RuntimeMemo returns the persisted runtime measurements; ok is false when
// none were saved.
func (s *Store) RuntimeMemo() (r RuntimeMemo, ok bool, err error) {
	ok, err = s.getDoc(keyRuntime, &r)
	return r, ok, err
}

// SaveRuntimeMemo persists the runtime measurements.
func (s *Store) SaveRuntimeMemo(r RuntimeMemo) error { return s.putDoc(keyRuntime, r) }

func (s *Store) getDoc(key string, v any) (bool, error) {
	var body string
	err := s.db.QueryRow(`SELECT body FROM documents WHERE key = ?`, key).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: read %s: %w", key, err)
	}
	if err := json.Unmarshal([]byte(body), v); err != nil {
		return false, fmt.Errorf("store: decode %s: %w", key, err)
	}
	return true, nil
}

func (s *Store) putDoc(key string, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("store: encode %s: %w", key, err)
	}
	_, err = s.db.Exec(`INSERT INTO documents (key, body, updated_ms) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET body = excluded.body, updated_ms = excluded.updated_ms`,
		key, string(body), ms(time.Now()))
	if err != nil {
		return fmt.Errorf("store: write %s: %w", key, err)
	}
	return nil
}
