package controller

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/events"
	"github.com/sentania-labs/benchwarmer/internal/policy"
	"github.com/sentania-labs/benchwarmer/internal/telemetry"
)

func TestAttributionEpisode(t *testing.T) {
	var log bytes.Buffer
	ev := &sink{}
	now := time.Unix(1000, 0)
	c := New(Deps{Events: ev, Now: func() time.Time { return now }, Log: slog.New(slog.NewTextHandler(&log, nil))})
	c.gpu.Confidence = policy.ConfidenceDegraded
	c.gpu.AttributionReasons = []string{"runtime PIDs absent from GPU process counters"}
	c.sample = telemetry.Sample{Complete: true, HasAdapterInstances: true, Processes: []telemetry.ProcGPU{{PID: 300, DedicatedBytes: 10}}}
	in := FactInput{RuntimePID: 100, OwnPIDs: []int{200, 100}, FootprintMiB: 4000}
	c.trackAttribution(now, in)
	e, ok := ev.find(events.TelemetryDegraded)
	if !ok || !strings.Contains(e.Message, "PIDs=[100 200]") || e.Data["duration_ms"] != int64(0) {
		t.Fatalf("onset: %+v", e)
	}
	c.trackAttribution(now.Add(time.Second), in)
	if ev.count(events.TelemetryDegraded) != 1 {
		t.Fatal("repeated unchanged sample logged")
	}
	c.sample.Complete = false
	c.sample.Errors = []string{"GPU Engine read failed"}
	c.gpu.AttributionReasons = []string{"GPU counter sample missing or incomplete"}
	c.trackAttribution(now.Add(2*time.Second), in)
	e, _ = ev.find(events.TelemetryDegraded)
	if e.Data["duration_ms"] != int64(2000) || !strings.Contains(log.String(), "GPU Engine read failed") {
		t.Fatalf("change: %+v log=%s", e, log.String())
	}
	c.gpu.Confidence = policy.ConfidenceHigh
	c.trackAttribution(now.Add(5*time.Second), in)
	e, _ = ev.find(events.TelemetryDegraded)
	if e.Data["status"] != "recovered" || e.Data["duration_ms"] != int64(5000) {
		t.Fatalf("recovery: %+v", e)
	}
	c.trackAttribution(now.Add(6*time.Second), in)
	if ev.count(events.TelemetryDegraded) != 3 {
		t.Fatal("recovery repeated")
	}
	c.gpu.Confidence = policy.ConfidenceDegraded
	c.trackAttribution(now.Add(7*time.Second), in)
	c.trackAttribution(now.Add(8*time.Second), FactInput{})
	e, _ = ev.find(events.TelemetryDegraded)
	if e.Data["status"] != "runtime_ended" || e.RuntimePID != 100 || e.Data["duration_ms"] != int64(1000) {
		t.Fatalf("ended: %+v", e)
	}
}

func TestAttributionMembershipOnlyUsesDebug(t *testing.T) {
	for _, level := range []slog.Level{slog.LevelInfo, slog.LevelDebug} {
		t.Run(level.String(), func(t *testing.T) {
			var log bytes.Buffer
			ev := &sink{}
			now := time.Unix(1000, 0)
			c := New(Deps{Events: ev, Now: func() time.Time { return now }, Log: slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: level}))})
			c.gpu.Confidence = policy.ConfidenceHigh
			c.sample = telemetry.Sample{Complete: true, Processes: []telemetry.ProcGPU{{PID: 100, DedicatedBytes: 4000 << 20}}}
			in := FactInput{RuntimePID: 100}
			c.trackAttribution(now, in)
			in.OwnPIDs = []int{100}
			c.trackAttribution(now.Add(time.Second), in)
			if ev.count(events.TelemetryDegraded) != 2 {
				t.Fatal("audit events lost")
			}
			if (log.Len() > 0) != (level == slog.LevelDebug) {
				t.Fatalf("unexpected log: %s", log.String())
			}
		})
	}
}

func TestAttributionCounterRecoveryWithMissingMembership(t *testing.T) {
	var log bytes.Buffer
	ev := &sink{}
	now := time.Unix(1000, 0)
	c := New(Deps{Events: ev, Now: func() time.Time { return now }, Log: slog.New(slog.NewTextHandler(&log, nil))})
	c.gpu.Confidence = policy.ConfidenceDegraded
	c.trackAttribution(now, FactInput{RuntimePID: 100})
	log.Reset()
	c.gpu.Confidence = policy.ConfidenceHigh
	c.trackAttribution(now.Add(3*time.Second), FactInput{RuntimePID: 100})
	if !strings.Contains(log.String(), "recovered after 3s") {
		t.Fatalf("counter recovery hidden: %s", log.String())
	}
	if ev.count(events.TelemetryDegraded) != 3 {
		t.Fatal("expected degradation, recovery, debug membership event")
	}
}
