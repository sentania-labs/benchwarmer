package policy

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/config"
	"github.com/sentania-labs/benchwarmer/internal/state"
)

var chicago, _ = time.LoadLocation("America/Chicago")

// Wednesday 2026-09-23 19:00 CDT: outside school hours, normal profile.
var evening = time.Date(2026, 9, 23, 19, 0, 0, 0, chicago)

// Wednesday 10:00 CDT: school hours.
var schoolDay = time.Date(2026, 9, 23, 10, 0, 0, 0, chicago)

func f64(v float64) *float64 { return &v }

// idle is a quiet desktop with the runtime stopped and every gate open.
func idle(now time.Time) Snapshot {
	return Snapshot{
		Now:     now,
		Mode:    ModeFacts{Mode: ModeAuto},
		Runtime: RuntimeFacts{State: state.Stopped},
		GPU: GPUFacts{
			Confidence: ConfidenceHigh, HealthyFor: time.Hour,
			TotalUtilPct: 3, ExternalUtilPct: 3, ExternalUtilTrusted: true, BelowSoftFor: time.Hour,
			VRAMTotalMiB: 16304, VRAMUsedMiB: 900, VRAMFreeMiB: 15404, ExternalVRAMMiB: 900,
			TemperatureC: f64(45),
		},
		Apps: AppFacts{ProcessListOK: true},
	}
}

// ready is the runtime loaded and idle with a quiet desktop.
func ready(now time.Time) Snapshot {
	s := idle(now)
	s.Runtime = RuntimeFacts{State: state.Ready, PID: 4242, FootprintMiB: 12500}
	s.GPU.VRAMUsedMiB, s.GPU.VRAMFreeMiB, s.GPU.OwnVRAMMiB = 13400, 2904, 12500
	return s
}

func busy(now time.Time) Snapshot {
	s := ready(now)
	s.Runtime.State = state.Busy
	s.Runtime.ActiveRequests = 1
	s.Runtime.OldestRequestAge = 4 * time.Second
	s.GPU.OwnUtilPct, s.GPU.TotalUtilPct = 98, 99
	return s
}

func game(name string) AppMatch {
	return AppMatch{PID: 7000, Name: name, Path: `D:\SteamLibrary\steamapps\common\X\` + name, Class: config.ClassGame, Rule: "Steam library"}
}

type want struct {
	action       Action
	rule         string
	idle         state.State
	grace        time.Duration
	competing    *bool
	suppress     *bool
	counts       *bool
	profile      string
	alsoHas      string
	nextLoadAt   time.Time
	nextLoadZero bool
}

func yes() *bool { b := true; return &b }
func no() *bool  { b := false; return &b }

func check(t *testing.T, d Decision, w want) {
	t.Helper()
	if d.Action != w.action || (w.rule != "" && d.Rule != w.rule) {
		t.Fatalf("got action=%s rule=%s (%s), want action=%s rule=%s", d.Action, d.Rule, d.Reason, w.action, w.rule)
	}
	if w.idle != "" && d.IdleState != w.idle {
		t.Errorf("idle state %s, want %s", d.IdleState, w.idle)
	}
	if w.grace != 0 && d.Grace != w.grace {
		t.Errorf("grace %s, want %s", d.Grace, w.grace)
	}
	if w.competing != nil && d.Competing != *w.competing {
		t.Errorf("competing %v, want %v", d.Competing, *w.competing)
	}
	if w.suppress != nil && d.Suppress != *w.suppress {
		t.Errorf("suppress %v, want %v", d.Suppress, *w.suppress)
	}
	if w.counts != nil && d.CountsAsPreemption != *w.counts {
		t.Errorf("counts-as-preemption %v, want %v", d.CountsAsPreemption, *w.counts)
	}
	if w.profile != "" && d.Profile != w.profile {
		t.Errorf("profile %s, want %s", d.Profile, w.profile)
	}
	if w.alsoHas != "" && !slices.Contains(d.AlsoMatched, w.alsoHas) {
		t.Errorf("also-matched %v lacks %s", d.AlsoMatched, w.alsoHas)
	}
	if !w.nextLoadAt.IsZero() && !d.NextLoadAt.Equal(w.nextLoadAt) {
		t.Errorf("next load %s, want %s", d.NextLoadAt, w.nextLoadAt)
	}
	if w.nextLoadZero && !d.NextLoadAt.IsZero() {
		t.Errorf("next load %s, want none", d.NextLoadAt)
	}
	if strings.Contains(d.Reason, "T19:") || strings.Contains(d.Reason, "-05:00") {
		t.Errorf("reason contains a raw timestamp: %q", d.Reason)
	}
	if d.Reason == "" {
		t.Error("empty reason")
	}
}

func TestPolicyTable(t *testing.T) {
	cfg := config.Default()
	cases := []struct {
		name string
		s    func() Snapshot
		want want
	}{
		// Normal operation.
		{"idle desktop loads", func() Snapshot { return idle(evening) },
			want{action: ActionRun, rule: RuleRun, profile: "normal"}},
		{"ready stays ready", func() Snapshot { return ready(evening) },
			want{action: ActionRun, rule: RuleRun}},
		{"own inference load is not contention", func() Snapshot {
			s := busy(evening)
			s.GPU.TotalUtilPct, s.GPU.OwnUtilPct, s.GPU.ExternalUtilPct = 100, 99, 2
			return s
		}, want{action: ActionRun, rule: RuleRun}},

		// Games.
		{"game launch while idle holds and marks competing", func() Snapshot {
			s := idle(evening)
			s.Apps.Games = []AppMatch{game("eldenring.exe")}
			return s
		}, want{action: ActionHold, rule: RuleGameProcess, idle: state.Cooldown, competing: yes()}},
		{"game process during inference drains with normal grace", func() Snapshot {
			s := busy(evening)
			s.Apps.Games = []AppMatch{game("eldenring.exe")}
			return s
		}, want{action: ActionDrain, rule: RuleGameProcess, grace: 15 * time.Second, counts: yes(), competing: yes()}},
		{"foreground game is confirmed gaming", func() Snapshot {
			s := busy(evening)
			g := game("eldenring.exe")
			g.Foreground = true
			s.Apps.Games = []AppMatch{g}
			return s
		}, want{action: ActionDrain, rule: RuleGameConfirmed, grace: 15 * time.Second, alsoHas: RuleGameProcess}},
		{"game with GPU use confirmed without session agent", func() Snapshot {
			s := busy(evening)
			g := game("eldenring.exe")
			g.GPUUtilPct = 35
			s.Apps.Games = []AppMatch{g}
			return s
		}, want{action: ActionDrain, rule: RuleGameConfirmed}},
		{"game under soft pressure gets shortened grace", func() Snapshot {
			s := busy(evening)
			g := game("eldenring.exe")
			g.VRAMMiB = 900
			s.Apps.Games = []AppMatch{g}
			s.GPU.ExternalUtilPct = 30
			return s
		}, want{action: ActionDrain, rule: RuleGameConfirmed, grace: 5 * time.Second}},

		// Critical contention beats grace.
		{"critical free VRAM with external growth preempts", func() Snapshot {
			s := busy(evening)
			s.GPU.VRAMFreeMiB, s.GPU.ExternalVRAMGrowthMiB, s.GPU.ExternalVRAMMiB = 300, 800, 1700
			return s
		}, want{action: ActionPreempt, rule: RuleCriticalVRAMFree, counts: yes()}},
		{"low free VRAM without external growth is not critical", func() Snapshot {
			s := busy(evening)
			s.GPU.VRAMFreeMiB, s.GPU.ExternalVRAMGrowthMiB = 300, 0
			return s
		}, want{action: ActionRun}},
		{"critical external VRAM preempts even with active request", func() Snapshot {
			s := busy(evening)
			s.GPU.ExternalVRAMMiB = 3500
			return s
		}, want{action: ActionPreempt, rule: RuleCriticalVRAMExt}},
		{"critical external utilization sustained preempts", func() Snapshot {
			s := busy(evening)
			s.GPU.ExternalUtilPct, s.GPU.AboveCriticalFor = 75, 4*time.Second
			return s
		}, want{action: ActionPreempt, rule: RuleCriticalUtil}},
		{"critical utilization spike shorter than window only drains", func() Snapshot {
			s := busy(evening)
			s.GPU.ExternalUtilPct, s.GPU.AboveCriticalFor, s.GPU.AboveSoftFor = 75, time.Second, 12*time.Second
			return s
		}, want{action: ActionDrain, rule: RuleSoftContention, grace: 5 * time.Second}},
		{"untrusted utilization cannot trigger critical util", func() Snapshot {
			s := busy(evening)
			s.GPU.Confidence, s.GPU.ExternalUtilTrusted = ConfidenceDegraded, false
			s.GPU.ExternalUtilPct, s.GPU.AboveCriticalFor = 99, time.Minute
			return s
		}, want{action: ActionRun}},
		{"critical contention outranks confirmed game", func() Snapshot {
			s := busy(evening)
			g := game("cyberpunk2077.exe")
			g.Foreground = true
			s.Apps.Games = []AppMatch{g}
			s.GPU.ExternalVRAMMiB = 4000
			return s
		}, want{action: ActionPreempt, rule: RuleCriticalVRAMExt, alsoHas: RuleGameConfirmed}},

		// Launchers.
		{"launcher alone never yields", func() Snapshot {
			s := ready(evening)
			s.Apps.Launchers = []AppMatch{{PID: 100, Name: "steam.exe", Class: config.ClassLauncher}}
			return s
		}, want{action: ActionRun, rule: RuleRun}},
		{"launcher alone never blocks load", func() Snapshot {
			s := idle(evening)
			s.Apps.Launchers = []AppMatch{{PID: 100, Name: "steam.exe", Class: config.ClassLauncher}}
			return s
		}, want{action: ActionRun}},
		{"launcher child using GPU is ambiguous in normal", func() Snapshot {
			s := ready(evening)
			s.Apps.LauncherChildren = []AppMatch{{PID: 7100, Name: "unknowngame.exe", ParentName: "steam.exe", GPUUtilPct: 5, VRAMMiB: 200}}
			return s
		}, want{action: ActionDrain, rule: RuleAmbiguous}},
		{"launcher child foreground is gaming", func() Snapshot {
			s := ready(evening)
			s.Apps.LauncherChildren = []AppMatch{{PID: 7100, Name: "unknowngame.exe", ParentName: "steam.exe", GPUUtilPct: 5, Foreground: true}}
			return s
		}, want{action: ActionDrain, rule: RuleLauncherChildGPU}},

		// Non-game GPU load.
		{"browser video below soft threshold keeps running", func() Snapshot {
			s := ready(evening)
			s.GPU.ExternalUtilPct, s.GPU.AboveSoftFor = 12, 0
			s.Apps.Ordinary = []AppMatch{{PID: 300, Name: "chrome.exe", Class: config.ClassOrdinary, GPUUtilPct: 12}}
			return s
		}, want{action: ActionRun}},
		{"sustained OBS load drains as soft contention", func() Snapshot {
			s := busy(evening)
			s.GPU.ExternalUtilPct, s.GPU.AboveSoftFor = 35, 15*time.Second
			s.Apps.Ordinary = []AppMatch{{PID: 300, Name: "obs64.exe", Class: config.ClassOrdinary, GPUUtilPct: 35}}
			return s
		}, want{action: ActionDrain, rule: RuleSoftContention, grace: 5 * time.Second}},
		{"fullscreen browser video is not unclassified fullscreen", func() Snapshot {
			s := ready(evening)
			s.Session = SessionFacts{Known: true, Fullscreen: true, ForegroundName: "chrome.exe", ForegroundClass: config.ClassOrdinary}
			s.GPU.ExternalUtilPct = 30
			return s
		}, want{action: ActionRun}},
		{"unclassified fullscreen with GPU use is strong interactive demand", func() Snapshot {
			s := ready(evening)
			s.Session = SessionFacts{Known: true, Fullscreen: true, ForegroundName: "indiegame.exe"}
			s.GPU.ExternalUtilPct = 40
			return s
		}, want{action: ActionDrain, rule: RuleFullscreenGPU}},

		// School hours.
		{"school hours: game process drains with 60s grace", func() Snapshot {
			s := busy(schoolDay)
			s.Apps.Games = []AppMatch{game("minecraft.exe")}
			return s
		}, want{action: ActionDrain, rule: RuleGameProcess, grace: 60 * time.Second, profile: "school_hours"}},
		{"school hours: unclassified fullscreen without GPU tolerated", func() Snapshot {
			s := ready(schoolDay)
			s.Session = SessionFacts{Known: true, Fullscreen: true, ForegroundName: "slides.exe"}
			return s
		}, want{action: ActionRun, profile: "school_hours"}},
		{"school hours: soft util at 30% tolerated", func() Snapshot {
			s := ready(schoolDay)
			s.GPU.ExternalUtilPct, s.GPU.AboveSoftFor = 30, 0
			return s
		}, want{action: ActionRun}},
		{"school hours: critical contention still preempts", func() Snapshot {
			s := busy(schoolDay)
			s.GPU.ExternalVRAMMiB = 3500
			return s
		}, want{action: ActionPreempt, rule: RuleCriticalVRAMExt}},

		// Manual modes.
		{"pause drains an active request", func() Snapshot {
			s := busy(evening)
			s.Mode = ModeFacts{Mode: ModePause, SetBy: "tray"}
			return s
		}, want{action: ActionDrain, rule: RulePause, idle: state.Disabled, counts: no(), competing: no()}},
		{"pause while stopped holds disabled", func() Snapshot {
			s := idle(evening)
			s.Mode = ModeFacts{Mode: ModePause}
			return s
		}, want{action: ActionHold, rule: RulePause, idle: state.Disabled}},
		{"AI priority ignores early-warning game process", func() Snapshot {
			s := busy(evening)
			s.Mode = ModeFacts{Mode: ModeAIPriority}
			s.Apps.Games = []AppMatch{game("eldenring.exe")}
			return s
		}, want{action: ActionRun, rule: RuleAIPriorityRun, profile: "ai_priority"}},
		{"AI priority ignores soft contention", func() Snapshot {
			s := busy(evening)
			s.Mode = ModeFacts{Mode: ModeAIPriority}
			s.GPU.ExternalUtilPct, s.GPU.AboveSoftFor = 45, time.Minute
			return s
		}, want{action: ActionRun, rule: RuleAIPriorityRun}},
		{"AI priority does not override confirmed gaming", func() Snapshot {
			s := busy(evening)
			s.Mode = ModeFacts{Mode: ModeAIPriority}
			g := game("eldenring.exe")
			g.Foreground = true
			s.Apps.Games = []AppMatch{g}
			return s
		}, want{action: ActionDrain, rule: RuleGameConfirmed, grace: 120 * time.Second}},
		{"safety overrides AI priority: temperature", func() Snapshot {
			s := busy(evening)
			s.Mode = ModeFacts{Mode: ModeAIPriority}
			s.GPU.TemperatureC = f64(93)
			return s
		}, want{action: ActionPreempt, rule: RuleTemperature, counts: no()}},
		{"critical contention overrides AI priority", func() Snapshot {
			s := busy(evening)
			s.Mode = ModeFacts{Mode: ModeAIPriority}
			s.GPU.ExternalVRAMMiB = 3500
			return s
		}, want{action: ActionPreempt, rule: RuleCriticalVRAMExt}},
		{"pause outranks AI-priority-level rules and critical contention", func() Snapshot {
			s := busy(evening)
			s.Mode = ModeFacts{Mode: ModePause}
			s.GPU.ExternalVRAMMiB = 3500
			return s
		}, want{action: ActionDrain, rule: RulePause, alsoHas: RuleCriticalVRAMExt}},

		// Safety.
		{"temperature at limit preempts", func() Snapshot {
			s := busy(evening)
			s.GPU.TemperatureC = f64(90)
			return s
		}, want{action: ActionPreempt, rule: RuleTemperature}},
		{"warm but below critical keeps running", func() Snapshot {
			s := busy(evening)
			s.GPU.TemperatureC = f64(85)
			return s
		}, want{action: ActionRun}},
		{"warm above resume blocks load", func() Snapshot {
			s := idle(evening)
			s.GPU.TemperatureC = f64(82)
			return s
		}, want{action: ActionHold, rule: RuleTemperature}},
		{"unknown temperature does not block", func() Snapshot {
			s := idle(evening)
			s.GPU.TemperatureC = nil
			return s
		}, want{action: ActionRun}},
		{"suspend preempts", func() Snapshot {
			s := busy(evening)
			s.Power.Suspending = true
			return s
		}, want{action: ActionPreempt, rule: RuleSuspend, counts: no()}},
		{"device lost preempts into error", func() Snapshot {
			s := ready(evening)
			s.Power.DeviceLost = true
			return s
		}, want{action: ActionPreempt, rule: RuleDeviceLost, idle: state.Error}},
		{"shutdown preempts", func() Snapshot {
			s := busy(evening)
			s.Power.ShuttingDown = true
			return s
		}, want{action: ActionPreempt, rule: RuleShutdown}},

		// Telemetry loss.
		{"brief telemetry loss keeps running", func() Snapshot {
			s := busy(evening)
			s.GPU.Confidence, s.GPU.NoneFor = ConfidenceNone, 3*time.Second
			return s
		}, want{action: ActionRun}},
		{"sustained telemetry loss releases the GPU", func() Snapshot {
			s := busy(evening)
			s.GPU.Confidence, s.GPU.NoneFor = ConfidenceNone, 11*time.Second
			return s
		}, want{action: ActionPreempt, rule: RuleTelemetryLost, idle: state.Error}},
		{"no telemetry blocks loading", func() Snapshot {
			s := idle(evening)
			s.GPU.Confidence, s.GPU.NoneFor = ConfidenceNone, 2*time.Second
			return s
		}, want{action: ActionHold, rule: RuleNoTelemetry}},
		{"degraded telemetry: any game process counts as confirmed", func() Snapshot {
			s := busy(evening)
			s.GPU.Confidence = ConfidenceDegraded
			s.Apps.Games = []AppMatch{game("eldenring.exe")}
			return s
		}, want{action: ActionDrain, rule: RuleGameConfirmed}},
		{"degraded telemetry tightens soft VRAM threshold", func() Snapshot {
			s := ready(evening)
			s.GPU.Confidence = ConfidenceDegraded
			s.GPU.ExternalVRAMMiB = 1700 // below 2048, above 2048*0.8
			return s
		}, want{action: ActionDrain, rule: RuleSoftContention}},
		{"process list failure blocks loading", func() Snapshot {
			s := idle(evening)
			s.Apps.ProcessListOK = false
			return s
		}, want{action: ActionHold, rule: RuleNoProcessList}},

		// Eligibility and timers.
		{"missing model holds as setup required", func() Snapshot {
			s := idle(evening)
			s.Runtime.SetupProblem = `model file C:\m.gguf not found`
			return s
		}, want{action: ActionHold, rule: RuleSetupRequired, idle: state.Stopped}},
		{"setup required outranks a recovery wait", func() Snapshot {
			s := idle(evening)
			s.Runtime.SetupProblem = "no model file is chosen"
			s.Timers.RecoveryUntil, s.Timers.RecoveryReason = evening.Add(time.Minute), "startup"
			return s
		}, want{action: ActionHold, rule: RuleSetupRequired}},
		{"a game still wins over setup required", func() Snapshot {
			s := idle(evening)
			s.Runtime.SetupProblem = "no model file is chosen"
			s.Apps.Games = []AppMatch{game("eldenring.exe")}
			return s
		}, want{action: ActionHold, rule: RuleGameProcess}},
		{"insufficient free VRAM holds", func() Snapshot {
			s := idle(evening)
			s.GPU.VRAMFreeMiB = 9000
			return s
		}, want{action: ActionHold, rule: RuleVRAMInsufficient, idle: state.Stopped}},
		{"cooldown holds until last competing + cooldown", func() Snapshot {
			s := idle(evening)
			s.Timers.LastCompetingAt, s.Timers.LastCompetingRule = evening.Add(-2*time.Minute), RuleGameProcess
			return s
		}, want{action: ActionHold, rule: RuleCooldown, idle: state.Cooldown, nextLoadAt: evening.Add(3 * time.Minute)}},
		{"cooldown expired loads", func() Snapshot {
			s := idle(evening)
			s.Timers.LastCompetingAt = evening.Add(-5*time.Minute - time.Second)
			return s
		}, want{action: ActionRun}},
		{"school-hours cooldown is shorter for the same event", func() Snapshot {
			s := idle(schoolDay)
			s.Timers.LastCompetingAt = schoolDay.Add(-3 * time.Minute)
			return s
		}, want{action: ActionRun, profile: "school_hours"}},
		{"competing event during cooldown is competing (resets cooldown)", func() Snapshot {
			s := idle(evening)
			s.Timers.LastCompetingAt = evening.Add(-4 * time.Minute)
			s.Apps.Games = []AppMatch{game("eldenring.exe")}
			return s
		}, want{action: ActionHold, rule: RuleGameProcess, competing: yes()}},
		{"hysteresis after util contention holds until clear window", func() Snapshot {
			s := idle(evening)
			s.Timers.LastCompetingAt, s.Timers.LastCompetingRule = evening.Add(-10*time.Minute), RuleSoftContention
			s.GPU.BelowSoftFor = 10 * time.Second
			return s
		}, want{action: ActionHold, rule: RuleSoftContention, competing: yes()}},
		{"no hysteresis at startup", func() Snapshot {
			s := idle(evening)
			s.GPU.BelowSoftFor = 0
			return s
		}, want{action: ActionRun}},
		{"suppression holds and outranks cooldown", func() Snapshot {
			s := idle(evening)
			s.Timers.SuppressedAt = evening.Add(-10 * time.Minute)
			s.Timers.LastCompetingAt = evening.Add(-time.Minute)
			return s
		}, want{action: ActionHold, rule: RuleSuppressed, idle: state.Suppressed, nextLoadAt: evening.Add(20 * time.Minute)}},
		{"suppression stays visible while the game keeps running", func() Snapshot {
			s := idle(evening)
			s.Timers.SuppressedAt = evening.Add(-5 * time.Minute)
			s.Apps.Games = []AppMatch{game("eldenring.exe")}
			return s
		}, want{action: ActionHold, rule: RuleGameProcess, idle: state.Suppressed}},
		{"no fixed next-load time while the game keeps running", func() Snapshot {
			s := idle(evening)
			s.Timers.LastCompetingAt = evening
			s.Apps.Games = []AppMatch{game("eldenring.exe")}
			return s
		}, want{action: ActionHold, rule: RuleGameProcess, nextLoadZero: true}},
		{"suppression expired loads", func() Snapshot {
			s := idle(evening)
			s.Timers.SuppressedAt = evening.Add(-31 * time.Minute)
			return s
		}, want{action: ActionRun}},
		{"second preemption in window suppresses", func() Snapshot {
			s := busy(evening)
			s.Timers.Preemptions = []time.Time{evening.Add(-6 * time.Minute)}
			s.Apps.Games = []AppMatch{game("eldenring.exe")}
			return s
		}, want{action: ActionDrain, suppress: yes(), counts: yes()}},
		{"preemption outside window does not suppress", func() Snapshot {
			s := busy(evening)
			s.Timers.Preemptions = []time.Time{evening.Add(-11 * time.Minute)}
			s.Apps.Games = []AppMatch{game("eldenring.exe")}
			return s
		}, want{action: ActionDrain, suppress: no()}},
		{"manual pause never counts toward suppression", func() Snapshot {
			s := busy(evening)
			s.Timers.Preemptions = []time.Time{evening.Add(-time.Minute)}
			s.Mode = ModeFacts{Mode: ModePause}
			return s
		}, want{action: ActionDrain, suppress: no(), counts: no()}},
		{"crash backoff holds in error", func() Snapshot {
			s := idle(evening)
			s.Timers.RecoveryUntil, s.Timers.RecoveryReason = evening.Add(time.Minute), "crash backoff (attempt 2)"
			return s
		}, want{action: ActionHold, rule: RuleRecovery, idle: state.Error, nextLoadAt: evening.Add(time.Minute)}},
		{"service restart applies startup cooldown", func() Snapshot {
			s := idle(evening)
			s.Timers.RecoveryUntil, s.Timers.RecoveryReason = evening.Add(90*time.Second), "startup"
			return s
		}, want{action: ActionHold, rule: RuleRecovery, idle: state.Cooldown}},
		{"resume cooldown holds", func() Snapshot {
			s := idle(evening)
			s.Timers.RecoveryUntil, s.Timers.RecoveryReason = evening.Add(2*time.Minute), "resume"
			return s
		}, want{action: ActionHold, rule: RuleRecovery}},
		{"yield rule outranks recovery wait", func() Snapshot {
			s := idle(evening)
			s.Timers.RecoveryUntil, s.Timers.RecoveryReason = evening.Add(time.Minute), "startup"
			s.GPU.ExternalVRAMMiB = 3500
			return s
		}, want{action: ActionHold, rule: RuleCriticalVRAMExt}},
		{"running runtime ignores eligibility gates", func() Snapshot {
			s := ready(evening)
			s.Timers.LastCompetingAt = evening.Add(-time.Minute)
			s.GPU.VRAMFreeMiB = 2900
			return s
		}, want{action: ActionRun}},
		{"loading runtime preempts rather than drains on critical", func() Snapshot {
			s := ready(evening)
			s.Runtime.State = state.Loading
			s.GPU.ExternalVRAMMiB = 3500
			return s
		}, want{action: ActionPreempt}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			check(t, Evaluate(tc.s(), cfg), tc.want)
		})
	}
}

func TestDeterministic(t *testing.T) {
	cfg := config.Default()
	s := busy(evening)
	s.Apps.Games = []AppMatch{game("a.exe"), game("b.exe")}
	s.GPU.ExternalVRAMMiB = 3500
	a := Evaluate(s, cfg)
	for i := 0; i < 50; i++ {
		b := Evaluate(s, cfg)
		if b.Rule != a.Rule || b.Reason != a.Reason || !slices.Equal(b.AlsoMatched, a.AlsoMatched) {
			t.Fatalf("non-deterministic: %+v vs %+v", a, b)
		}
	}
}

func TestEveryDecisionHasEvidenceWhenYielding(t *testing.T) {
	cfg := config.Default()
	for _, s := range []Snapshot{
		func() Snapshot { s := busy(evening); s.GPU.ExternalVRAMMiB = 3500; return s }(),
		func() Snapshot { s := busy(evening); s.Apps.Games = []AppMatch{game("x.exe")}; return s }(),
		func() Snapshot { s := busy(evening); s.GPU.TemperatureC = f64(95); return s }(),
	} {
		d := Evaluate(s, cfg)
		if !d.Action.Yields() || len(d.Evidence) == 0 {
			t.Errorf("%s: yields=%v evidence=%v", d.Rule, d.Action.Yields(), d.Evidence)
		}
	}
}

func TestDSTScheduleBoundaryInPolicy(t *testing.T) {
	cfg := config.Default()
	// Monday after spring-forward: 06:59 CDT normal, 07:00 CDT school hours.
	before := time.Date(2027, 3, 15, 11, 59, 0, 0, time.UTC)
	after := time.Date(2027, 3, 15, 12, 0, 0, 0, time.UTC)
	if d := Evaluate(idle(before), cfg); d.Profile != "normal" {
		t.Fatalf("before: %s", d.Profile)
	}
	if d := Evaluate(idle(after), cfg); d.Profile != "school_hours" || d.ProfileSource != "schedule:school-hours" {
		t.Fatalf("after: %s %s", d.Profile, d.ProfileSource)
	}
}
