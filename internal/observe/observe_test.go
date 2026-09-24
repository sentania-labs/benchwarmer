package observe

import (
	"testing"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/config"
	"github.com/sentania-labs/benchwarmer/internal/policy"
	"github.com/sentania-labs/benchwarmer/internal/signals"
	"github.com/sentania-labs/benchwarmer/internal/telemetry"
)

var t0 = time.Date(2026, 9, 23, 20, 0, 0, 0, time.UTC)

const (
	pidRuntime = 100
	pidChild   = 101
	pidDWM     = 10
	pidSteam   = 20
	pidGame    = 30
	pidChrome  = 40
)

// proc is a per-process GPU entry: util on the 3D engine, dedicated MiB.
type proc struct {
	pid  uint32
	util float64
	mib  uint64
	eng  string // engine key; default phys_0_eng_0 (3D)
}

// sample builds a complete sample with a 16 GiB adapter. adapterMiB is the
// adapter-wide dedicated usage. Engine totals are derived from procs.
func sample(adapterMiB uint64, procs ...proc) *telemetry.Sample {
	s := &telemetry.Sample{
		Complete:            true,
		HasAdapterInstances: true,
		DedicatedTotalBytes: 16384 * mib,
		DedicatedUsedBytes:  adapterMiB * mib,
		Engines: []telemetry.Engine{
			{Key: "phys_0_eng_0", Type: "3D"},
			{Key: "phys_0_eng_1", Type: "Compute"},
		},
	}
	for _, p := range procs {
		k := p.eng
		if k == "" {
			k = "phys_0_eng_0"
		}
		typ := "3D"
		if k == "phys_0_eng_1" {
			typ = "Compute"
		}
		s.Processes = append(s.Processes, telemetry.ProcGPU{
			PID: p.pid, EngineByKey: map[string]float64{k: p.util},
			EngineUtil: map[string]float64{typ: p.util}, DedicatedBytes: p.mib * mib,
		})
		for i := range s.Engines {
			if s.Engines[i].Key == k {
				s.Engines[i].UtilPct += p.util
			}
		}
	}
	return s
}

// adapterOnly builds a complete sample with engine totals but no per-process
// data (degraded attribution).
func adapterOnly(adapterMiB uint64, util float64) *telemetry.Sample {
	s := sample(adapterMiB)
	s.Engines[0].UtilPct = util
	return s
}

func desktop() []signals.Process {
	return []signals.Process{
		{PID: pidDWM, PPID: 1, Name: "dwm.exe", Path: `C:\Windows\System32\dwm.exe`, CreateTime: t0.Add(-time.Hour)},
		{PID: pidSteam, PPID: 1, Name: "steam.exe", Path: `C:\Program Files (x86)\Steam\steam.exe`, CreateTime: t0.Add(-time.Hour)},
		{PID: pidChrome, PPID: 1, Name: "chrome.exe", Path: `C:\Program Files\Google\Chrome\Application\chrome.exe`, CreateTime: t0.Add(-time.Hour)},
	}
}

func withRuntime(ps []signals.Process) []signals.Process {
	return append(ps,
		signals.Process{PID: pidRuntime, PPID: 1, Name: "llama-server.exe", Path: `C:\Benchwarmer\llama\llama-server.exe`},
		signals.Process{PID: pidChild, PPID: pidRuntime, Name: "chrome.exe", Path: `C:\x\chrome.exe`})
}

func input(s *telemetry.Sample, ps []signals.Process) Input {
	c := config.Default()
	return Input{Sample: s, Processes: ps, Config: c, Profile: c.Profiles["normal"]}
}

func running(in Input, active bool) Input {
	in.OwnPIDs = []int{pidRuntime, pidChild}
	in.RuntimeActive = active
	in.FootprintMiB = 12000
	return in
}

func TestIdleDesktop(t *testing.T) {
	o := New()
	var f Facts
	for i := 0; i < 5; i++ {
		f = o.Observe(t0.Add(time.Duration(i)*time.Second),
			input(sample(900, proc{pidDWM, 4, 300, ""}, proc{pidChrome, 0.5, 20, ""}, proc{pidSteam, 0, 150, ""}), desktop()))
	}
	g := f.GPU
	if g.Confidence != policy.ConfidenceHigh || !g.ExternalUtilTrusted {
		t.Fatalf("confidence %s trusted %v", g.Confidence, g.ExternalUtilTrusted)
	}
	if g.ExternalUtilPct != 4.5 || g.OwnUtilPct != 0 || g.ExternalVRAMMiB != 900 || g.VRAMFreeMiB != 16384-900 {
		t.Fatalf("gpu facts %+v", g)
	}
	if g.AboveSoftFor != 0 || g.AboveCriticalFor != 0 || g.BelowSoftFor != 4*time.Second || g.HealthyFor != 4*time.Second {
		t.Fatalf("windows %+v", g)
	}
	a := f.Apps
	if !a.ProcessListOK || len(a.Games) != 0 || len(a.Ordinary) != 0 || len(a.LauncherChildren) != 0 {
		t.Fatalf("apps %+v", a)
	}
	if len(a.Launchers) != 1 || a.Launchers[0].Name != "steam.exe" || a.Launchers[0].VRAMMiB != 150 || a.Launchers[0].RunningFor != time.Hour+4*time.Second {
		t.Fatalf("launchers %+v", a.Launchers)
	}
}

func TestOwnInferenceNotExternal(t *testing.T) {
	o := New()
	var f Facts
	for i := 0; i < 5; i++ {
		s := sample(12900, proc{pidRuntime, 95, 12000, "phys_0_eng_1"}, proc{pidChild, 0, 0, ""}, proc{pidDWM, 3, 300, ""})
		f = o.Observe(t0.Add(time.Duration(i)*time.Second), running(input(s, withRuntime(desktop())), true))
	}
	g := f.GPU
	if g.Confidence != policy.ConfidenceHigh || !g.ExternalUtilTrusted {
		t.Fatalf("confidence %s trusted %v", g.Confidence, g.ExternalUtilTrusted)
	}
	if g.OwnUtilPct != 95 || g.ExternalUtilPct != 3 || g.TotalUtilPct != 95 {
		t.Fatalf("util %+v", g)
	}
	// External VRAM prefers the adapter figure minus own usage.
	if g.OwnVRAMMiB != 12000 || g.ExternalVRAMMiB != 900 || g.AboveSoftFor != 0 || g.AboveCriticalFor != 0 {
		t.Fatalf("vram/windows %+v", g)
	}
	// The runtime's own processes never appear in app facts, even when a
	// rule would match them (the child is named chrome.exe).
	for _, m := range f.Apps.Ordinary {
		if m.PID == pidRuntime || m.PID == pidChild {
			t.Fatalf("own process listed: %+v", m)
		}
	}
}

func TestGameLoadingDipVersusSustained(t *testing.T) {
	o := New()
	c := config.Default()
	// External util per second: loading bursts with a dip, then sustained.
	utils := []float64{70, 70, 20, 70, 70, 70, 70}
	var crit, soft, below []time.Duration
	for i, u := range utils {
		f := o.Observe(t0.Add(time.Duration(i)*time.Second), input(sample(3000, proc{pidGame, u, 2500, ""}), desktop()))
		crit = append(crit, f.GPU.AboveCriticalFor)
		soft = append(soft, f.GPU.AboveSoftFor)
		below = append(below, f.GPU.BelowSoftFor)
	}
	s := time.Second
	wantCrit := []time.Duration{0, s, 0, 0, s, 2 * s, 3 * s}
	wantSoft := []time.Duration{0, s, 0, 0, s, 2 * s, 3 * s}
	wantBelow := []time.Duration{0, 0, 0, 0, 0, 0, 0}
	for i := range utils {
		if crit[i] != wantCrit[i] || soft[i] != wantSoft[i] || below[i] != wantBelow[i] {
			t.Fatalf("tick %d: crit %v soft %v below %v", i, crit, soft, below)
		}
	}
	// The dip broke the run, so the critical window was not reached until
	// the final tick.
	if crit[3] >= c.Contention.CriticalWindow.D() || crit[6] < c.Contention.CriticalWindow.D() {
		t.Fatalf("critical window: %v", crit)
	}
}

func TestDegradedFallback(t *testing.T) {
	o := New()
	// No per-process data; runtime loaded and idle; adapter at 13500 MiB.
	in := running(input(adapterOnly(13500, 30), withRuntime(desktop())), false)
	f := o.Observe(t0, in)
	f = o.Observe(t0.Add(time.Second), in)
	g := f.GPU
	if g.Confidence != policy.ConfidenceDegraded {
		t.Fatalf("confidence %s", g.Confidence)
	}
	if g.ExternalVRAMMiB != 1500 || g.OwnVRAMMiB != 12000 {
		t.Fatalf("vram %+v", g)
	}
	if !g.ExternalUtilTrusted || g.ExternalUtilPct != 30 {
		t.Fatalf("idle runtime: util should be trusted: %+v", g)
	}
	// Degraded tightens soft 25% by 20% to 20%, so 30% is above soft.
	if g.AboveSoftFor != time.Second {
		t.Fatalf("above soft %v", g.AboveSoftFor)
	}

	// Busy: utilization cannot be split; untrusted, windows reset.
	f = o.Observe(t0.Add(2*time.Second), running(input(adapterOnly(13500, 90), withRuntime(desktop())), true))
	g = f.GPU
	if g.ExternalUtilTrusted || g.AboveSoftFor != 0 || g.AboveCriticalFor != 0 || g.BelowSoftFor != 0 {
		t.Fatalf("busy degraded: %+v", g)
	}

	// Footprint below usage floors external at 0; runtime stopped means all
	// adapter usage is external.
	in = running(input(adapterOnly(11000, 0), withRuntime(desktop())), false)
	if g := o.Observe(t0.Add(3*time.Second), in).GPU; g.ExternalVRAMMiB != 0 || g.OwnVRAMMiB != 11000 {
		t.Fatalf("floor: %+v", g)
	}
	if g := o.Observe(t0.Add(4*time.Second), input(adapterOnly(900, 0), desktop())).GPU; g.ExternalVRAMMiB != 900 || g.OwnVRAMMiB != 0 {
		t.Fatalf("stopped: %+v", g)
	}
}

func TestDegradedFirstLoadUsesRequiredVRAM(t *testing.T) {
	in := running(input(adapterOnly(14000, 90), withRuntime(desktop())), true)
	in.FootprintMiB = 0
	g := New().Observe(t0, in).GPU
	want := 14000 - in.Config.Runtime.RequiredFreeVRAMMiB
	if g.ExternalVRAMMiB != want {
		t.Fatalf("external %d, want %d", g.ExternalVRAMMiB, want)
	}
}

func TestConfidenceGrades(t *testing.T) {
	tests := []struct {
		name string
		in   Input
		want policy.Confidence
	}{
		{"missing sample", input(nil, desktop()), policy.ConfidenceNone},
		{"incomplete sample", func() Input {
			s := sample(900, proc{pidDWM, 4, 300, ""})
			s.Complete = false
			return input(s, desktop())
		}(), policy.ConfidenceNone},
		{"adapter only", input(adapterOnly(900, 5), desktop()), policy.ConfidenceDegraded},
		{"per-process sum inconsistent", input(sample(9000, proc{pidDWM, 4, 2000, ""}), desktop()), policy.ConfidenceDegraded},
		{"runtime running but not visible", running(input(sample(12500, proc{pidDWM, 4, 12400, ""}), withRuntime(desktop())), false), policy.ConfidenceDegraded},
		{"consistent", input(sample(2000, proc{pidDWM, 4, 1500, ""}), desktop()), policy.ConfidenceHigh},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := New().Observe(t0, tt.in).GPU
			if g.Confidence != tt.want {
				t.Fatalf("confidence %s, want %s", g.Confidence, tt.want)
			}
			if tt.want == policy.ConfidenceNone && (g.ExternalUtilTrusted || g.ExternalVRAMMiB != 0) {
				t.Fatalf("none must carry no facts: %+v", g)
			}
		})
	}
}

func TestTelemetryDropout(t *testing.T) {
	o := New()
	good := func() Input { return input(sample(900, proc{pidGame, 70, 800, ""}), desktop()) }
	temp := 71.0
	bad := good()
	bad.Sample.Complete = false
	bad.Sample.TemperatureC = &temp

	steps := []struct {
		in                       Input
		conf                     policy.Confidence
		noneFor, healthy, aboveC time.Duration
	}{
		{good(), policy.ConfidenceHigh, 0, 0, 0},
		{good(), policy.ConfidenceHigh, 0, time.Second, time.Second},
		{bad, policy.ConfidenceNone, 0, 0, 0},
		{input(nil, desktop()), policy.ConfidenceNone, time.Second, 0, 0},
		{input(nil, nil), policy.ConfidenceNone, 2 * time.Second, 0, 0},
		{good(), policy.ConfidenceHigh, 0, 0, 0},
		{good(), policy.ConfidenceHigh, 0, time.Second, time.Second},
	}
	for i, st := range steps {
		f := o.Observe(t0.Add(time.Duration(i)*time.Second), st.in)
		g := f.GPU
		if g.Confidence != st.conf || g.NoneFor != st.noneFor || g.HealthyFor != st.healthy || g.AboveCriticalFor != st.aboveC {
			t.Fatalf("step %d: %+v", i, g)
		}
		if i == 2 && (g.TemperatureC == nil || *g.TemperatureC != 71) {
			t.Fatalf("temperature must survive an incomplete sample: %+v", g.TemperatureC)
		}
		if i == 4 && f.Apps.ProcessListOK {
			t.Fatal("nil process list must report ProcessListOK false")
		}
	}
}

func TestGapBreaksContinuity(t *testing.T) {
	o := New()
	in := input(sample(900, proc{pidGame, 70, 800, ""}), desktop())
	o.Observe(t0, in)
	o.Observe(t0.Add(time.Second), in)
	g := o.Observe(t0.Add(time.Minute), in).GPU
	if g.AboveCriticalFor != 0 || g.HealthyFor != 0 {
		t.Fatalf("gap must reset windows: %+v", g)
	}
	// Clock moving backwards also resets.
	o.Observe(t0.Add(time.Minute+time.Second), in)
	if g := o.Observe(t0, in).GPU; g.AboveCriticalFor != 0 {
		t.Fatalf("backwards clock: %+v", g)
	}
}

func TestExternalVRAMGrowth(t *testing.T) {
	o := New()
	// Window is 3 s. External VRAM ramps 500 -> 3500 MiB.
	var growth []int
	for i, ext := range []uint64{500, 500, 1500, 2500, 3500, 3500, 3500, 3500} {
		f := o.Observe(t0.Add(time.Duration(i)*time.Second), input(sample(ext, proc{pidGame, 5, ext, ""}), desktop()))
		growth = append(growth, f.GPU.ExternalVRAMGrowthMiB)
	}
	want := []int{0, 0, 1000, 2000, 3000, 2000, 1000, 0}
	for i := range want {
		if growth[i] != want[i] {
			t.Fatalf("growth %v, want %v", growth, want)
		}
	}
}

func TestSessionFreshness(t *testing.T) {
	game := signals.Process{PID: pidGame, PPID: 1, Name: "eldenring.exe", Path: `D:\SteamLibrary\steamapps\common\ELDEN RING\Game\eldenring.exe`}
	rep := &AgentReport{ForegroundPID: pidGame, ForegroundName: game.Name, ForegroundPath: game.Path, Fullscreen: true, IdleSeconds: 2.5, Locked: false}
	ps := append(desktop(), game)

	in := input(sample(3000, proc{pidGame, 5, 2000, ""}), ps)
	in.Agent, in.AgentAt = rep, t0.Add(-5*time.Second)
	f := New().Observe(t0, in)
	s := f.Session
	if !s.Known || !s.UserLoggedOn || s.ForegroundClass != config.ClassGame || !s.Fullscreen || s.IdleFor != 2500*time.Millisecond {
		t.Fatalf("fresh session %+v", s)
	}
	if len(f.Apps.Games) != 1 || !f.Apps.Games[0].Foreground || !f.Apps.Games[0].Fullscreen {
		t.Fatalf("game foreground %+v", f.Apps.Games)
	}

	in.AgentAt = t0.Add(-in.Config.Signals.SessionStaleAfter.D())
	f = New().Observe(t0, in)
	if f.Session.Known || f.Apps.Games[0].Foreground || f.Apps.Games[0].Fullscreen {
		t.Fatalf("stale report must be unknown: %+v %+v", f.Session, f.Apps.Games[0])
	}

	in.Agent = &AgentReport{ForegroundName: "mystery.exe", ForegroundPath: `C:\Apps\mystery.exe`, Fullscreen: true}
	in.AgentAt = t0
	if s := New().Observe(t0, in).Session; !s.Known || s.ForegroundClass != "" {
		t.Fatalf("unclassified foreground %+v", s)
	}
}

func TestLauncherChildren(t *testing.T) {
	steamStart := t0.Add(-time.Hour)
	ps := []signals.Process{
		{PID: pidSteam, PPID: 1, Name: "steam.exe", Path: `C:\Program Files (x86)\Steam\steam.exe`, CreateTime: steamStart},
		// Unlisted game outside any library folder, started by Steam.
		{PID: 50, PPID: pidSteam, Name: "indie.exe", Path: `E:\Games\Indie\indie.exe`, CreateTime: t0.Add(-time.Minute)},
		// Steam child with negligible GPU use (e.g. a crash handler).
		{PID: 51, PPID: pidSteam, Name: "helper.exe", Path: `C:\x\helper.exe`, CreateTime: t0.Add(-time.Minute)},
		// Child of a non-launcher.
		{PID: 52, PPID: pidDWM, Name: "other.exe", Path: `C:\x\other.exe`, CreateTime: t0.Add(-time.Minute)},
		{PID: pidDWM, PPID: 1, Name: "dwm.exe", Path: `C:\Windows\System32\dwm.exe`, CreateTime: steamStart},
		// PPID points at Steam's PID but the process predates Steam: reuse.
		{PID: 53, PPID: pidSteam, Name: "old.exe", Path: `C:\x\old.exe`, CreateTime: steamStart.Add(-time.Hour)},
		// VRAM alone above the launcher-child floor.
		{PID: 54, PPID: pidSteam, Name: "vram.exe", Path: `C:\x\vram.exe`, CreateTime: t0.Add(-time.Minute)},
	}
	s := sample(4000, proc{50, 40, 2500, ""}, proc{51, 0.2, 10, ""}, proc{52, 50, 500, ""}, proc{53, 50, 500, ""}, proc{54, 0, 200, ""}, proc{pidDWM, 3, 200, ""})
	a := New().Observe(t0, input(s, ps)).Apps
	if len(a.LauncherChildren) != 2 {
		t.Fatalf("launcher children %+v", a.LauncherChildren)
	}
	c := a.LauncherChildren[0]
	if c.PID != 50 || c.ParentName != "steam.exe" || c.Rule != RuleLauncherChild || c.GPUUtilPct != 40 || c.VRAMMiB != 2500 || c.RunningFor != time.Minute {
		t.Fatalf("child %+v", c)
	}
	if a.LauncherChildren[1].PID != 54 {
		t.Fatalf("second child %+v", a.LauncherChildren[1])
	}
}

func TestIgnoredAndOrdinaryFiltering(t *testing.T) {
	ps := []signals.Process{
		{PID: pidDWM, Name: "dwm.exe", Path: `C:\Windows\System32\dwm.exe`},
		{PID: 41, Name: "chrome.exe", Path: `C:\c\chrome.exe`},
		{PID: 42, Name: "chrome.exe", Path: `C:\c\chrome.exe`},
		{PID: 43, Name: "obs64.exe", Path: `C:\o\obs64.exe`},
		{PID: 44, Name: "vc_redist.x64.exe", Path: `C:\Steam\steamapps\common\Steamworks Shared\_CommonRedist\vc_redist.x64.exe`},
		{PID: 45, Name: "notepad.exe", Path: `C:\Windows\notepad.exe`},
	}
	s := sample(2000, proc{pidDWM, 40, 500, ""}, proc{41, 0.5, 20, ""}, proc{42, 5, 30, ""}, proc{43, 0, 100, ""}, proc{44, 30, 300, ""}, proc{45, 30, 300, ""})
	a := New().Observe(t0, input(s, ps)).Apps
	if len(a.Games)+len(a.Launchers)+len(a.LauncherChildren) != 0 {
		t.Fatalf("unexpected %+v", a)
	}
	// dwm and the redistributable are ignored; chrome 41 has no measurable
	// use; notepad is unclassified and not a launcher child.
	if len(a.Ordinary) != 2 || a.Ordinary[0].PID != 42 || a.Ordinary[1].PID != 43 {
		t.Fatalf("ordinary %+v", a.Ordinary)
	}
}

func TestClassifierFollowsConfig(t *testing.T) {
	o := New()
	ps := []signals.Process{{PID: 60, Name: "custom.exe", Path: `C:\custom.exe`}}
	in := input(sample(900), ps)
	if a := o.Observe(t0, in).Apps; len(a.Games) != 0 {
		t.Fatalf("games %+v", a.Games)
	}
	in.Config.Applications = append([]config.AppRule{{Name: "custom", Exe: "custom.exe", Class: config.ClassGame}}, in.Config.Applications...)
	if a := o.Observe(t0.Add(time.Second), in).Apps; len(a.Games) != 1 || a.Games[0].Rule != "custom" {
		t.Fatalf("games %+v", a.Games)
	}
}
