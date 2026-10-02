package service

import (
	"testing"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/config"
	"github.com/sentania-labs/benchwarmer/internal/controller"
	"github.com/sentania-labs/benchwarmer/internal/policy"
	"github.com/sentania-labs/benchwarmer/internal/signals"
	"github.com/sentania-labs/benchwarmer/internal/state"
	"github.com/sentania-labs/benchwarmer/internal/telemetry"
)

func TestMissingRuntimeMembershipDoesNotPreemptInference(t *testing.T) {
	const root = 100
	const mib = 1 << 20
	cfg := config.Default()
	zone, _ := time.LoadLocation("America/Chicago")
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, zone)
	f := NewFacts()
	sample := telemetry.Sample{Complete: true, HasAdapterInstances: true, DedicatedTotalBytes: 16200 * mib, DedicatedUsedBytes: 15072 * mib,
		Processes: []telemetry.ProcGPU{{PID: root, DedicatedBytes: 14344 * mib}, {PID: 10, DedicatedBytes: 1064 * mib}}}
	in := controller.FactInput{Sample: &sample, Procs: []signals.Process{{PID: root, Name: "llama-server.exe"}}, RuntimePID: root, RuntimeBusy: true, FootprintMiB: 14344, Config: cfg, Profile: cfg.Profiles["normal"]}
	for i := range 30 {
		gpu, apps, session := f.Build(now.Add(time.Duration(i)*time.Second), in)
		s := policy.Snapshot{Now: now, Mode: policy.ModeFacts{Mode: policy.ModeAuto}, Runtime: policy.RuntimeFacts{State: state.Busy, ActiveRequests: 1}, GPU: gpu, Apps: apps, Session: session}
		d := policy.Evaluate(s, cfg)
		if d.Action != policy.ActionRun {
			t.Fatalf("membership gap interrupted inference: %+v", d)
		}
	}
	sample.Processes[1].DedicatedBytes = 4000 * mib
	sample.Processes[0].DedicatedBytes = 11000 * mib
	sample.DedicatedUsedBytes = 15000 * mib
	gpu, apps, session := f.Build(now.Add(31*time.Second), in)
	d := policy.Evaluate(policy.Snapshot{Now: now, Mode: policy.ModeFacts{Mode: policy.ModeAuto}, Runtime: policy.RuntimeFacts{State: state.Busy, ActiveRequests: 1}, GPU: gpu, Apps: apps, Session: session}, cfg)
	if d.Action != policy.ActionPreempt || d.Rule != policy.RuleCriticalVRAMExt {
		t.Fatalf("real contention not protected: %+v", d)
	}
}

func TestReloadStillPreemptsForMeasuredExternalMemory(t *testing.T) {
	const mib = 1 << 20
	cfg := config.Default()
	zone, _ := time.LoadLocation("America/Chicago")
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, zone)
	sample := telemetry.Sample{Complete: true, HasAdapterInstances: true, DedicatedTotalBytes: 16200 * mib, DedicatedUsedBytes: 4000 * mib,
		Processes: []telemetry.ProcGPU{{PID: 100, DedicatedBytes: 0}, {PID: 10, DedicatedBytes: 4000 * mib}}}
	in := controller.FactInput{Sample: &sample, RuntimePID: 100, OwnPIDs: []int{100}, RuntimeBusy: true, FootprintMiB: 14344, Config: cfg, Profile: cfg.Profiles["school_hours"]}
	gpu, apps, session := NewFacts().Build(now, in)
	d := policy.Evaluate(policy.Snapshot{Now: now, Mode: policy.ModeFacts{Mode: policy.ModeAuto}, Runtime: policy.RuntimeFacts{State: state.Loading}, GPU: gpu, Apps: apps, Session: session}, cfg)
	if d.Action != policy.ActionPreempt || d.Rule != policy.RuleCriticalVRAMExt {
		t.Fatalf("reload hid real contention: %+v", d)
	}
}
