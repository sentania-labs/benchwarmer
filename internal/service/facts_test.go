package service

import (
	"testing"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/api"
	"github.com/sentania-labs/benchwarmer/internal/config"
	"github.com/sentania-labs/benchwarmer/internal/controller"
	"github.com/sentania-labs/benchwarmer/internal/policy"
	"github.com/sentania-labs/benchwarmer/internal/signals"
	"github.com/sentania-labs/benchwarmer/internal/state"
	"github.com/sentania-labs/benchwarmer/internal/telemetry"
	"github.com/sentania-labs/benchwarmer/internal/winsvc"
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

func TestSessionLockNotificationsOverrideTray(t *testing.T) {
	cfg := config.Default()
	cfg.Applications = nil // Exercise installations without lock-screen ignore rules.
	now := time.Date(2026, 10, 3, 23, 0, 0, 0, time.UTC)
	f := NewFacts()
	in := controller.FactInput{Config: cfg, Profile: cfg.Profiles["normal"]}
	report := api.AgentReport{SessionID: 7, ForegroundName: "LockApp.exe", Fullscreen: true}
	check := func(want bool) {
		t.Helper()
		_, _, session := f.Build(now, in)
		if !session.Known || session.Locked != want {
			t.Fatalf("session = %+v, want locked=%v", session, want)
		}
		// Use the built session facts with a healthy, loaded runtime to prove
		// the ambiguous-fullscreen rule cannot unload it while WTS says locked.
		d := policy.Evaluate(policy.Snapshot{Now: now, Mode: policy.ModeFacts{Mode: policy.ModeAuto},
			Runtime: policy.RuntimeFacts{State: state.Ready}, Session: session,
			GPU: policy.GPUFacts{Confidence: policy.ConfidenceHigh, HealthyFor: time.Hour, ExternalUtilTrusted: true,
				VRAMTotalMiB: 16000, VRAMFreeMiB: 10000}, Apps: policy.AppFacts{ProcessListOK: true}}, cfg)
		if want && d.Action != policy.ActionRun {
			t.Fatalf("locked desktop yielded: %+v", d)
		}
		if !want && d.Rule != policy.RuleAmbiguous {
			t.Fatalf("unlocked fullscreen did not yield: %+v", d)
		}
	}
	f.AgentReport(now, report)
	check(false) // Before the first notification, use the tray report.
	f.SessionChange(winsvc.SessionLock, 7)
	check(true) // Existing report is overridden immediately.
	f.AgentReport(now, report)
	check(true) // Subsequent false tray reports cannot clear the lock.
	f.SessionChange(winsvc.SessionUnlock, 8)
	check(true) // Other sessions cannot change this one.
	report.Locked = true
	f.AgentReport(now, report)
	f.SessionChange(winsvc.SessionUnlock, 7)
	check(false) // Explicit unlock also overrides a stale true tray value.
	f.SessionChange(winsvc.SessionLock, 8)
	report.SessionID, report.Locked = 9, false
	f.AgentReport(now, report)
	check(false) // A session with no notification still uses the tray.
	report.Locked = true
	f.AgentReport(now, report)
	check(true)
}

func TestSessionLockStateClearedAtSessionBoundaries(t *testing.T) {
	for _, kind := range []winsvc.SessionKind{winsvc.SessionLogoff, winsvc.SessionLogon} {
		t.Run(string(kind), func(t *testing.T) {
			now := time.Now()
			cfg := config.Default()
			f := NewFacts()
			f.SessionChange(winsvc.SessionLock, 7)
			f.SessionChange(kind, 7)
			f.AgentReport(now, api.AgentReport{SessionID: 7, Locked: false})
			_, _, session := f.Build(now, controller.FactInput{Config: cfg, Profile: cfg.Profiles["normal"]})
			if !session.Known || session.Locked {
				t.Fatalf("reused session inherited old lock: %+v", session)
			}
		})
	}
}
