package metrics

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sentania-labs/benchwarmer/internal/policy"
	"github.com/sentania-labs/benchwarmer/internal/state"
)

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	b, _ := io.ReadAll(rec.Body)
	return string(b)
}

func TestExposition(t *testing.T) {
	m := New()
	m.SetBuildInfo("v1.2.3")
	m.SetState(state.Busy)
	m.IncTransition(state.Ready, state.Busy)
	m.IncLoad("ok")
	m.IncLoad("bogus")
	m.ObserveLoadSeconds(42)
	m.SetActiveRequests(2)
	m.ObserveRequestSeconds("ok", 3.5)
	m.IncRequestsRejected()
	m.IncDrain("grace_expired")
	m.IncPreemption(policy.RuleGameConfirmed, true)
	m.IncPreemption("Some Game.exe", false)
	m.IncCooldown()
	m.IncSuppression()
	m.IncRuntimeCrash()
	m.IncTelemetryFailure()
	m.IncOrphansKilled(2)
	m.IncConfigChange("applied")
	temp := 71.0
	m.SetGPU(policy.GPUFacts{Confidence: policy.ConfidenceDegraded, TotalUtilPct: 80, OwnUtilPct: 70, ExternalUtilPct: 10,
		VRAMUsedMiB: 14000, VRAMFreeMiB: 2000, OwnVRAMMiB: 12800, ExternalVRAMMiB: 1200, TemperatureC: &temp})
	m.ObserveVRAMReleaseSeconds(1.5)
	m.SetNextLoadSeconds(-5)
	var dropped uint64 = 7
	m.ObserveEventSink(func() uint64 { return dropped }, func() uint64 { return 0 })

	out := scrape(t, m)
	lines := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		lines[l] = true
	}
	for _, want := range []string{
		`benchwarmer_build_info{version="v1.2.3"} 1`,
		`benchwarmer_state{state="BUSY"} 1`,
		`benchwarmer_state{state="READY"} 0`,
		`benchwarmer_condition{condition="Available"} 1`,
		`benchwarmer_condition{condition="Unavailable"} 0`,
		`benchwarmer_ready 1`,
		`benchwarmer_state_transitions_total{from="READY",to="BUSY"} 1`,
		`benchwarmer_state_transitions_total{from="STOPPED",to="LOADING"} 0`,
		`benchwarmer_model_loads_total{result="ok"} 1`,
		`benchwarmer_model_loads_total{result="other"} 1`,
		`benchwarmer_model_loads_total{result="timeout"} 0`,
		`benchwarmer_model_load_duration_seconds_count 1`,
		`benchwarmer_model_load_duration_seconds_sum 42`,
		`benchwarmer_active_requests 2`,
		`benchwarmer_request_duration_seconds_count{outcome="ok"} 1`,
		`benchwarmer_request_duration_seconds_count{outcome="force_closed"} 0`,
		`benchwarmer_requests_rejected_total 1`,
		`benchwarmer_drains_total{outcome="grace_expired"} 1`,
		`benchwarmer_drains_total{outcome="completed"} 0`,
		`benchwarmer_preemptions_total{forced="true",rule="gaming.confirmed"} 1`,
		`benchwarmer_preemptions_total{forced="false",rule="other"} 1`,
		`benchwarmer_cooldowns_total 1`,
		`benchwarmer_suppressions_total 1`,
		`benchwarmer_runtime_crashes_total 1`,
		`benchwarmer_telemetry_failures_total 1`,
		`benchwarmer_orphans_killed_total 2`,
		`benchwarmer_config_changes_total{result="applied"} 1`,
		`benchwarmer_config_changes_total{result="rejected"} 0`,
		`benchwarmer_gpu_utilization_percent{scope="external"} 10`,
		`benchwarmer_gpu_vram_mib{kind="own"} 12800`,
		`benchwarmer_gpu_vram_mib{kind="free"} 2000`,
		`benchwarmer_gpu_temperature_celsius 71`,
		`benchwarmer_telemetry_confidence{confidence="degraded"} 1`,
		`benchwarmer_telemetry_confidence{confidence="high"} 0`,
		`benchwarmer_vram_release_seconds_count 1`,
		`benchwarmer_next_load_seconds 0`,
		`benchwarmer_events_dropped_total 7`,
	} {
		if !lines[want] {
			t.Errorf("missing %q", want)
		}
	}
	if !strings.Contains(out, "\ngo_goroutines ") {
		t.Error("missing Go runtime metrics")
	}
}

func TestTemperatureUnknownIsNaN(t *testing.T) {
	m := New()
	m.SetGPU(policy.GPUFacts{Confidence: policy.ConfidenceNone})
	if out := scrape(t, m); !strings.Contains(out, "benchwarmer_gpu_temperature_celsius NaN\n") {
		t.Fatal("unknown temperature should be NaN")
	}
}

func TestPrivateRegistry(t *testing.T) {
	// Two instances must not collide (the global registry would panic).
	a, b := New(), New()
	a.IncCooldown()
	if strings.Contains(scrape(t, b), "benchwarmer_cooldowns_total 1") {
		t.Fatal("instances share state")
	}
}
