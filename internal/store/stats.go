package store

import (
	"database/sql"
	"fmt"
	"time"
)

// StatsDelta is an increment to one hourly bucket.
type StatsDelta struct {
	Loads int
	// LoadSecondsTotal is added to the bucket total; LoadSecondsMax raises
	// the bucket maximum if larger. LoadDelta fills both for one load.
	LoadSecondsTotal   float64
	LoadSecondsMax     float64
	AvailableSeconds   float64
	UnavailableSeconds float64
	Drains             int
	ForcedPreemptions  int
	Crashes            int
}

// LoadDelta is the delta for one completed load that took seconds.
func LoadDelta(seconds float64) StatsDelta {
	return StatsDelta{Loads: 1, LoadSecondsTotal: seconds, LoadSecondsMax: seconds}
}

// HourStats is one hourly bucket. Hour is the bucket start.
type HourStats struct {
	Hour               time.Time `json:"hour"`
	Loads              int       `json:"loads"`
	LoadSecondsTotal   float64   `json:"load_seconds_total"`
	LoadSecondsMax     float64   `json:"load_seconds_max"`
	AvailableSeconds   float64   `json:"available_seconds"`
	UnavailableSeconds float64   `json:"unavailable_seconds"`
	Drains             int       `json:"drains"`
	ForcedPreemptions  int       `json:"forced_preemptions"`
	Crashes            int       `json:"crashes"`
}

// hourKey is the bucket for t: Unix seconds of the hour start. Hours are
// absolute, so a daylight-saving change never merges or splits a bucket.
func hourKey(t time.Time) int64 { return t.Unix() - t.Unix()%3600 }

// AddStats adds d to the bucket containing at.
func (s *Store) AddStats(at time.Time, d StatsDelta) error {
	return s.tx(func(tx *sql.Tx) error { return addStats(tx, hourKey(at), d) })
}

// AddAvailability records the span from..to as available or unavailable,
// split across hour buckets at hour boundaries.
func (s *Store) AddAvailability(from, to time.Time, available bool) error {
	if !to.After(from) {
		return nil
	}
	return s.tx(func(tx *sql.Tx) error {
		for t := from; t.Before(to); {
			next := time.Unix(hourKey(t)+3600, 0)
			if next.After(to) {
				next = to
			}
			sec := next.Sub(t).Seconds()
			d := StatsDelta{UnavailableSeconds: sec}
			if available {
				d = StatsDelta{AvailableSeconds: sec}
			}
			if err := addStats(tx, hourKey(t), d); err != nil {
				return err
			}
			t = next
		}
		return nil
	})
}

func addStats(tx *sql.Tx, hour int64, d StatsDelta) error {
	_, err := tx.Exec(`INSERT INTO stats_hourly (hour, loads, load_seconds_total, load_seconds_max,
			available_seconds, unavailable_seconds, drains, forced_preemptions, crashes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (hour) DO UPDATE SET
			loads = loads + excluded.loads,
			load_seconds_total = load_seconds_total + excluded.load_seconds_total,
			load_seconds_max = max(load_seconds_max, excluded.load_seconds_max),
			available_seconds = available_seconds + excluded.available_seconds,
			unavailable_seconds = unavailable_seconds + excluded.unavailable_seconds,
			drains = drains + excluded.drains,
			forced_preemptions = forced_preemptions + excluded.forced_preemptions,
			crashes = crashes + excluded.crashes`,
		hour, d.Loads, d.LoadSecondsTotal, d.LoadSecondsMax, d.AvailableSeconds, d.UnavailableSeconds,
		d.Drains, d.ForcedPreemptions, d.Crashes)
	if err != nil {
		return fmt.Errorf("store: add stats: %w", err)
	}
	return nil
}

// RecentStats returns the buckets for the last n hours up to and including
// the hour containing now, oldest first. Hours with no activity are absent.
func (s *Store) RecentStats(now time.Time, n int) ([]HourStats, error) {
	if n <= 0 {
		return []HourStats{}, nil
	}
	first := hourKey(now) - int64(n-1)*3600
	rows, err := s.db.Query(`SELECT hour, loads, load_seconds_total, load_seconds_max, available_seconds,
			unavailable_seconds, drains, forced_preemptions, crashes
		FROM stats_hourly WHERE hour >= ? AND hour <= ? ORDER BY hour`, first, hourKey(now))
	if err != nil {
		return nil, fmt.Errorf("store: query stats: %w", err)
	}
	defer rows.Close()
	out := []HourStats{}
	for rows.Next() {
		var h HourStats
		var hour int64
		if err := rows.Scan(&hour, &h.Loads, &h.LoadSecondsTotal, &h.LoadSecondsMax, &h.AvailableSeconds,
			&h.UnavailableSeconds, &h.Drains, &h.ForcedPreemptions, &h.Crashes); err != nil {
			return nil, fmt.Errorf("store: scan stats: %w", err)
		}
		h.Hour = time.Unix(hour, 0)
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: query stats: %w", err)
	}
	return out, nil
}

// PruneStats deletes buckets that ended more than maxAge before now. A zero
// maxAge keeps everything.
func (s *Store) PruneStats(now time.Time, maxAge time.Duration) (int64, error) {
	if maxAge <= 0 {
		return 0, nil
	}
	res, err := s.db.Exec(`DELETE FROM stats_hourly WHERE hour + 3600 <= ?`, now.Add(-maxAge).Unix())
	if err != nil {
		return 0, fmt.Errorf("store: prune stats: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
