package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/events"
)

// MaxEventsLimit caps one Events query. It is one more than the API's
// largest page so the API's look-ahead row (used to decide whether an older
// page exists) is never truncated away.
const MaxEventsLimit = 1001

// EventQuery selects events newest first. Limit <= 0 means 100; it is capped
// at MaxEventsLimit. BeforeID > 0 returns only events with a smaller ID (for
// paging backwards). Types, when non-empty, restricts the event types.
type EventQuery struct {
	Limit    int
	BeforeID int64
	Types    []events.Type
}

// AppendEvent stores one event and returns its assigned ID.
func (s *Store) AppendEvent(e events.Event) (int64, error) {
	ids, err := s.AppendEvents([]events.Event{e})
	if err != nil {
		return 0, err
	}
	return ids[0], nil
}

// AppendEvents stores events in one transaction and returns their IDs in
// order. Any ID already set on an event is ignored; the store assigns IDs.
func (s *Store) AppendEvents(evs []events.Event) ([]int64, error) {
	ids := make([]int64, 0, len(evs))
	err := s.tx(func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(`INSERT INTO events (time_ms, type, body) VALUES (?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, e := range evs {
			e.ID = 0
			if e.Time.IsZero() {
				e.Time = time.Now()
			}
			body, err := json.Marshal(e)
			if err != nil {
				return fmt.Errorf("encode %s event: %w", e.Type, err)
			}
			res, err := stmt.Exec(ms(e.Time), string(e.Type), string(body))
			if err != nil {
				return err
			}
			id, err := res.LastInsertId()
			if err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("store: append events: %w", err)
	}
	return ids, nil
}

// Events returns events newest first (by ID, which follows insertion order).
func (s *Store) Events(q EventQuery) ([]events.Event, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, MaxEventsLimit)

	var where []string
	var args []any
	if q.BeforeID > 0 {
		where = append(where, "id < ?")
		args = append(args, q.BeforeID)
	}
	if len(q.Types) > 0 {
		ph := make([]string, len(q.Types))
		for i, t := range q.Types {
			ph[i] = "?"
			args = append(args, string(t))
		}
		where = append(where, "type IN ("+strings.Join(ph, ",")+")")
	}
	query := "SELECT id, body FROM events"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query events: %w", err)
	}
	defer rows.Close()
	out := []events.Event{}
	for rows.Next() {
		var id int64
		var body string
		if err := rows.Scan(&id, &body); err != nil {
			return nil, fmt.Errorf("store: scan event: %w", err)
		}
		var e events.Event
		if err := json.Unmarshal([]byte(body), &e); err != nil {
			return nil, fmt.Errorf("store: decode event %d: %w", id, err)
		}
		e.ID = id
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: query events: %w", err)
	}
	return out, nil
}

// PruneEvents deletes events older than maxAge before now, then all but the
// newest maxCount. A zero maxAge or maxCount skips that bound. It returns
// how many events were removed.
func (s *Store) PruneEvents(now time.Time, maxAge time.Duration, maxCount int) (int64, error) {
	var n int64
	err := s.tx(func(tx *sql.Tx) error {
		if maxAge > 0 {
			res, err := tx.Exec(`DELETE FROM events WHERE time_ms < ?`, ms(now.Add(-maxAge)))
			if err != nil {
				return err
			}
			k, _ := res.RowsAffected()
			n += k
		}
		if maxCount > 0 {
			res, err := tx.Exec(`DELETE FROM events WHERE id <= (SELECT id FROM events ORDER BY id DESC LIMIT 1 OFFSET ?)`, maxCount)
			if err != nil {
				return err
			}
			k, _ := res.RowsAffected()
			n += k
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("store: prune events: %w", err)
	}
	return n, nil
}
