package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/config"
)

// ConfigChange is one config history row. Config is always redacted.
type ConfigChange struct {
	ID     int64         `json:"id"`
	Time   time.Time     `json:"time"`
	Actor  string        `json:"actor"`
	Config config.Config `json:"config"`
	Impact config.Impact `json:"impact"`
}

// AppendConfigChange records an applied config. The config is redacted here,
// not by the caller, so a secret cannot reach the history by omission.
func (s *Store) AppendConfigChange(at time.Time, actor string, c config.Config, im config.Impact) (int64, error) {
	cb, err := json.Marshal(config.Redact(c))
	if err != nil {
		return 0, fmt.Errorf("store: encode config: %w", err)
	}
	ib, err := json.Marshal(im)
	if err != nil {
		return 0, fmt.Errorf("store: encode impact: %w", err)
	}
	res, err := s.db.Exec(`INSERT INTO config_history (time_ms, actor, config, impact) VALUES (?, ?, ?, ?)`,
		ms(at), actor, string(cb), string(ib))
	if err != nil {
		return 0, fmt.Errorf("store: append config change: %w", err)
	}
	return res.LastInsertId()
}

// ConfigChanges returns up to limit recent changes, newest first.
func (s *Store) ConfigChanges(limit int) ([]ConfigChange, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT id, time_ms, actor, config, impact FROM config_history ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: query config history: %w", err)
	}
	defer rows.Close()
	out := []ConfigChange{}
	for rows.Next() {
		var c ConfigChange
		var t int64
		var cb, ib string
		if err := rows.Scan(&c.ID, &t, &c.Actor, &cb, &ib); err != nil {
			return nil, fmt.Errorf("store: scan config history: %w", err)
		}
		c.Time = time.UnixMilli(t)
		if err := json.Unmarshal([]byte(cb), &c.Config); err != nil {
			return nil, fmt.Errorf("store: decode config history %d: %w", c.ID, err)
		}
		if err := json.Unmarshal([]byte(ib), &c.Impact); err != nil {
			return nil, fmt.Errorf("store: decode config history %d: %w", c.ID, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: query config history: %w", err)
	}
	return out, nil
}

// maxConfigHistory bounds config history independent of age; configs are a
// few KiB each.
const maxConfigHistory = 1000

// PruneConfigHistory deletes changes older than maxAge and all but the newest
// maxConfigHistory. A zero maxAge applies only the count bound.
func (s *Store) PruneConfigHistory(now time.Time, maxAge time.Duration) (int64, error) {
	cutoff := int64(0)
	if maxAge > 0 {
		cutoff = ms(now.Add(-maxAge))
	}
	res, err := s.db.Exec(`DELETE FROM config_history WHERE time_ms < ?
		OR id <= (SELECT id FROM config_history ORDER BY id DESC LIMIT 1 OFFSET ?)`,
		cutoff, maxConfigHistory)
	if err != nil {
		return 0, fmt.Errorf("store: prune config history: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// Prune applies the configured retention to every table. The service calls
// it hourly (ADR 0008).
func (s *Store) Prune(now time.Time, r config.Retention) error {
	age := time.Duration(r.EventsDays) * 24 * time.Hour
	if _, err := s.PruneEvents(now, age, r.EventsMax); err != nil {
		return err
	}
	if _, err := s.PruneStats(now, age); err != nil {
		return err
	}
	_, err := s.PruneConfigHistory(now, age)
	return err
}
