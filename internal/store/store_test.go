package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/config"
	"github.com/sentania-labs/benchwarmer/internal/events"
	"github.com/sentania-labs/benchwarmer/internal/policy"
	"github.com/sentania-labs/benchwarmer/internal/state"
)

var t0 = time.Date(2026, 9, 23, 14, 30, 0, 0, time.FixedZone("CDT", -5*3600))

func open(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func tmpDB(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "benchwarmer.db")
	s := open(t, path)
	t.Cleanup(func() { s.Close() })
	return s, path
}

func ev(typ events.Type, at time.Time, msg string) events.Event {
	return events.Event{Time: at, Type: typ, Message: msg, State: state.Ready, PrevState: state.Loading}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "benchwarmer.db")
	s := open(t, path)
	id, err := s.AppendEvent(events.Event{
		Time: t0, Type: events.StateChanged, PrevState: state.Loading, State: state.Ready,
		Condition: state.Available, Rule: "default", Message: "loaded",
		Evidence: []policy.Evidence{{Name: "vram_free_mib", Value: "14000", Threshold: "13312"}},
		Data:     map[string]any{"load_ms": 41000.0},
	})
	if err != nil || id != 1 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	until := t0.Add(time.Hour)
	if err := s.SaveModeState(ModeState{Mode: policy.ModePause, Until: &until, SetBy: "tray", SetAt: t0}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTimers(Timers{Preemptions: []time.Time{t0}, SuppressedAt: t0, LastCompetingRule: "gaming", CrashCount: 2, BackoffAttempt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRuntimeMemo(RuntimeMemo{FootprintMiB: 12800, LastLoadSeconds: 41.5, MeasuredAt: t0}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddStats(t0, LoadDelta(41.5)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendConfigChange(t0, "api", config.Default(), config.Impact{Changed: []string{"runtime"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = open(t, path)
	defer s.Close()
	evs, err := s.Events(EventQuery{})
	if err != nil || len(evs) != 1 {
		t.Fatalf("events=%v err=%v", evs, err)
	}
	e := evs[0]
	if e.ID != 1 || !e.Time.Equal(t0) || e.State != state.Ready || e.Evidence[0].Threshold != "13312" || e.Data["load_ms"] != 41000.0 {
		t.Fatalf("event round trip: %+v", e)
	}
	m, ok, err := s.ModeState()
	if err != nil || !ok || m.Mode != policy.ModePause || !m.Until.Equal(until) || m.SetBy != "tray" {
		t.Fatalf("mode=%+v ok=%v err=%v", m, ok, err)
	}
	tm, ok, err := s.Timers()
	if err != nil || !ok || len(tm.Preemptions) != 1 || tm.CrashCount != 2 || tm.LastCompetingRule != "gaming" {
		t.Fatalf("timers=%+v ok=%v err=%v", tm, ok, err)
	}
	rm, ok, err := s.RuntimeMemo()
	if err != nil || !ok || rm.FootprintMiB != 12800 || rm.LastLoadSeconds != 41.5 {
		t.Fatalf("runtime=%+v ok=%v err=%v", rm, ok, err)
	}
	st, err := s.RecentStats(t0, 1)
	if err != nil || len(st) != 1 || st[0].Loads != 1 {
		t.Fatalf("stats=%+v err=%v", st, err)
	}
	ch, err := s.ConfigChanges(10)
	if err != nil || len(ch) != 1 || ch[0].Actor != "api" || ch[0].Impact.Changed[0] != "runtime" {
		t.Fatalf("history=%+v err=%v", ch, err)
	}
	// IDs keep increasing after reopen.
	if id, _ := s.AppendEvent(ev(events.LoadStarted, t0, "x")); id != 2 {
		t.Fatalf("next id %d", id)
	}
}

func TestMissingDocuments(t *testing.T) {
	s, _ := tmpDB(t)
	if _, ok, err := s.ModeState(); ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if _, ok, err := s.Timers(); ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

func TestMigrationIdempotent(t *testing.T) {
	s, path := tmpDB(t)
	if v, err := s.version(); err != nil || v != SchemaVersion() {
		t.Fatalf("version=%d err=%v", v, err)
	}
	// Running migrate again, and reopening, must be no-ops.
	if err := s.migrate(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = open(t, path)
	defer s.Close()
	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_version`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("schema_version rows=%d err=%v", rows, err)
	}
	if v, _ := s.version(); v != SchemaVersion() {
		t.Fatalf("version=%d", v)
	}
}

func TestRefusesNewerSchema(t *testing.T) {
	s, path := tmpDB(t)
	if _, err := s.db.Exec(`UPDATE schema_version SET version = ?`, SchemaVersion()+1); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s2, err := Open(path); err == nil {
		s2.Close()
		t.Fatal("opened a database from a newer build")
	}
}

func TestWALMode(t *testing.T) {
	s, _ := tmpDB(t)
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("mode=%q err=%v", mode, err)
	}
	var busy int
	if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil || busy != busyTimeoutMS {
		t.Fatalf("busy_timeout=%d err=%v", busy, err)
	}
}

func TestEventsPagingAndFilter(t *testing.T) {
	s, _ := tmpDB(t)
	for i := range 10 {
		typ := events.StateChanged
		if i%2 == 1 {
			typ = events.Preempted
		}
		if _, err := s.AppendEvent(ev(typ, t0.Add(time.Duration(i)*time.Second), fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.Events(EventQuery{Limit: 3})
	if err != nil || len(page) != 3 || page[0].ID != 10 || page[2].ID != 8 {
		t.Fatalf("page1=%v err=%v", ids(page), err)
	}
	page, _ = s.Events(EventQuery{Limit: 3, BeforeID: 8})
	if len(page) != 3 || page[0].ID != 7 || page[2].ID != 5 {
		t.Fatalf("page2=%v", ids(page))
	}
	page, _ = s.Events(EventQuery{Types: []events.Type{events.Preempted}})
	if len(page) != 5 || page[0].ID != 10 {
		t.Fatalf("filtered=%v", ids(page))
	}
	page, _ = s.Events(EventQuery{Types: []events.Type{events.Preempted, events.StateChanged}, BeforeID: 3})
	if len(page) != 2 {
		t.Fatalf("both types before 3=%v", ids(page))
	}
	page, _ = s.Events(EventQuery{Types: []events.Type{events.ModeChanged}})
	if page == nil || len(page) != 0 {
		t.Fatalf("empty result should be an empty slice, got %#v", page)
	}
}

func ids(evs []events.Event) []int64 {
	out := make([]int64, len(evs))
	for i, e := range evs {
		out[i] = e.ID
	}
	return out
}

func TestPruneByAgeAndCount(t *testing.T) {
	s, _ := tmpDB(t)
	// 5 events 40 days old, 20 recent.
	for i := range 5 {
		s.AppendEvent(ev(events.StateChanged, t0.Add(-40*24*time.Hour+time.Duration(i)*time.Minute), "old"))
	}
	for i := range 20 {
		s.AppendEvent(ev(events.StateChanged, t0.Add(-time.Duration(20-i)*time.Minute), "new"))
	}
	n, err := s.PruneEvents(t0, 30*24*time.Hour, 0)
	if err != nil || n != 5 {
		t.Fatalf("age prune removed %d err=%v", n, err)
	}
	n, err = s.PruneEvents(t0, 0, 12)
	if err != nil || n != 8 {
		t.Fatalf("count prune removed %d err=%v", n, err)
	}
	evs, _ := s.Events(EventQuery{Limit: 1000})
	if len(evs) != 12 || evs[0].ID != 25 || evs[11].ID != 14 {
		t.Fatalf("kept %v", ids(evs))
	}
	// Under the bound: nothing to do.
	if n, _ := s.PruneEvents(t0, 30*24*time.Hour, 100); n != 0 {
		t.Fatalf("removed %d", n)
	}
}

func TestConcurrentAppend(t *testing.T) {
	s, _ := tmpDB(t)
	const workers, each = 8, 50
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for i := range each {
				if _, err := s.AppendEvent(ev(events.StateChanged, t0, fmt.Sprint(w, i))); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	// Concurrent readers too.
	wg.Go(func() {
		for range 20 {
			if _, err := s.Events(EventQuery{Limit: 10}); err != nil {
				t.Error(err)
				return
			}
		}
	})
	wg.Wait()
	evs, err := s.Events(EventQuery{Limit: MaxEventsLimit})
	if err != nil || len(evs) != workers*each {
		t.Fatalf("got %d events err=%v", len(evs), err)
	}
	seen := map[int64]bool{}
	for _, e := range evs {
		if seen[e.ID] {
			t.Fatalf("duplicate id %d", e.ID)
		}
		seen[e.ID] = true
	}
}

func TestSinkFlushesOnClose(t *testing.T) {
	s, _ := tmpDB(t)
	sink := NewEventSink(s, 1000)
	for i := range 500 {
		sink.Emit(ev(events.StateChanged, t0, fmt.Sprint(i)))
	}
	sink.Close()
	sink.Close() // idempotent
	evs, err := s.Events(EventQuery{Limit: MaxEventsLimit})
	if err != nil || len(evs) != 500 || sink.Dropped() != 0 {
		t.Fatalf("stored %d dropped %d err=%v", len(evs), sink.Dropped(), err)
	}
	if evs[0].Message != "499" || evs[499].Message != "0" {
		t.Fatalf("order not preserved: first=%q last=%q", evs[0].Message, evs[499].Message)
	}
	sink.Emit(ev(events.StateChanged, t0, "late"))
	if sink.Dropped() != 1 {
		t.Fatalf("emit after close should drop, dropped=%d", sink.Dropped())
	}
}

func TestSinkNeverBlocks(t *testing.T) {
	s, _ := tmpDB(t)
	// Hold the only connection so the writer stalls on its first insert.
	conn, err := s.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	sink := NewEventSink(s, 4)
	done := make(chan struct{})
	go func() {
		for i := range 1000 {
			sink.Emit(ev(events.StateChanged, t0, fmt.Sprint(i)))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Emit blocked while the writer was stalled")
	}
	// At most the buffer plus one in-flight batch can have been accepted.
	if d := sink.Dropped(); d < 1000-4-maxBatch {
		t.Fatalf("dropped=%d, expected most events dropped", d)
	}
	conn.Close()
	sink.Close()
	evs, _ := s.Events(EventQuery{Limit: MaxEventsLimit})
	if uint64(len(evs))+sink.Dropped() != 1000 {
		t.Fatalf("stored %d + dropped %d != 1000", len(evs), sink.Dropped())
	}
}

func TestSinkCountsWriteFailures(t *testing.T) {
	s, _ := tmpDB(t)
	sink := NewEventSink(s, 10)
	var mu sync.Mutex
	var gotErr error
	sink.OnError(func(err error) { mu.Lock(); gotErr = err; mu.Unlock() })
	s.Close()
	sink.Emit(ev(events.StateChanged, t0, "x"))
	sink.Close()
	mu.Lock()
	defer mu.Unlock()
	if sink.Failed() != 1 || gotErr == nil {
		t.Fatalf("failed=%d err=%v", sink.Failed(), gotErr)
	}
}

func TestStatsBuckets(t *testing.T) {
	s, _ := tmpDB(t)
	h := t0.Truncate(time.Hour)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.AddStats(h.Add(5*time.Minute), LoadDelta(30)))
	must(s.AddStats(h.Add(10*time.Minute), LoadDelta(50)))
	must(s.AddStats(h.Add(20*time.Minute), StatsDelta{Drains: 1, ForcedPreemptions: 1, Crashes: 1}))
	// 50 minutes available spanning into the next hour.
	must(s.AddAvailability(h.Add(40*time.Minute), h.Add(90*time.Minute), true))
	must(s.AddAvailability(h.Add(90*time.Minute), h.Add(100*time.Minute), false))
	// Something two hours ago that a 2-hour window excludes.
	must(s.AddStats(h.Add(-2*time.Hour), StatsDelta{Crashes: 5}))

	now := h.Add(100 * time.Minute)
	got, err := s.RecentStats(now, 2)
	if err != nil || len(got) != 2 {
		t.Fatalf("stats=%+v err=%v", got, err)
	}
	a, b := got[0], got[1]
	if !a.Hour.Equal(h) || a.Loads != 2 || a.LoadSecondsTotal != 80 || a.LoadSecondsMax != 50 ||
		a.Drains != 1 || a.ForcedPreemptions != 1 || a.Crashes != 1 || a.AvailableSeconds != 20*60 {
		t.Fatalf("first hour %+v", a)
	}
	if !b.Hour.Equal(h.Add(time.Hour)) || b.AvailableSeconds != 30*60 || b.UnavailableSeconds != 10*60 {
		t.Fatalf("second hour %+v", b)
	}
	if all, _ := s.RecentStats(now, 24); len(all) != 3 {
		t.Fatalf("24h window has %d buckets", len(all))
	}
	n, err := s.PruneStats(now, 90*time.Minute)
	if err != nil || n != 1 {
		t.Fatalf("pruned %d err=%v", n, err)
	}
}

func TestConfigHistoryRedacts(t *testing.T) {
	s, _ := tmpDB(t)
	c := config.Default()
	c.Runtime.Args = []string{"--api-key", "hunter2", "--threads", "8"}
	if _, err := s.AppendConfigChange(t0, "api", c, config.Impact{}); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := s.db.QueryRow(`SELECT config FROM config_history`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	ch, _ := s.ConfigChanges(1)
	if len(ch) != 1 || ch[0].Config.Runtime.Args[1] != config.Redacted || strings.Contains(raw, "hunter2") {
		t.Fatalf("secret persisted: %s", raw)
	}
}

func TestPruneAll(t *testing.T) {
	s, _ := tmpDB(t)
	old := t0.Add(-40 * 24 * time.Hour)
	s.AppendEvent(ev(events.StateChanged, old, "old"))
	s.AddStats(old, StatsDelta{Crashes: 1})
	s.AppendConfigChange(old, "api", config.Default(), config.Impact{})
	s.AppendEvent(ev(events.StateChanged, t0, "new"))
	if err := s.Prune(t0, config.Default().Retention); err != nil {
		t.Fatal(err)
	}
	evs, _ := s.Events(EventQuery{})
	st, _ := s.RecentStats(t0, 24*60)
	ch, _ := s.ConfigChanges(10)
	if len(evs) != 1 || len(st) != 0 || len(ch) != 0 {
		t.Fatalf("events=%d stats=%d history=%d", len(evs), len(st), len(ch))
	}
}
