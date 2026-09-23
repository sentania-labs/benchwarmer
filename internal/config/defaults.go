package config

import "time"

func d(v time.Duration) Duration { return Duration(v) }

// Default returns the shipped configuration. Threshold values are the
// provisional starting points in docs/phase0/thresholds.md, not universal
// truths; tune them from target-machine evidence.
func Default() Config {
	return Config{
		SchemaVersion: SchemaVersion,
		Runtime: Runtime{
			Executable:              `C:\Benchwarmer\llama\llama-server.exe`,
			ModelPath:               `C:\Benchwarmer\models\model.gguf`,
			Args:                    []string{},
			ContextSize:             8192,
			GPULayers:               999,
			Host:                    "127.0.0.1",
			Port:                    18481,
			LoadTimeout:             d(180 * time.Second),
			RequiredFreeVRAMMiB:     13312,
			KillVerifyTimeout:       d(10 * time.Second),
			VRAMReleaseTimeout:      d(15 * time.Second),
			VRAMReleaseToleranceMiB: 256,
			MaxRequestDuration:      d(10 * time.Minute),
			DiagnosticsTailBytes:    16384,
		},
		Listen: Listen{
			Inference:  "127.0.0.1:8480",
			Management: "127.0.0.1:8481",
		},
		Telemetry: Telemetry{
			SampleInterval:    d(time.Second),
			LossGrace:         d(10 * time.Second),
			DegradedMarginPct: 20,
		},
		Safety: Safety{GPUTempCriticalC: 90, GPUTempResumeC: 80},
		Contention: Contention{
			VRAMFreeCriticalMiB:     512,
			ExternalVRAMCriticalMiB: 3072,
			ExternalUtilCriticalPct: 60,
			CriticalWindow:          d(3 * time.Second),
			ClearWindow:             d(30 * time.Second),
		},
		Profiles: map[string]Profile{
			"normal": {
				Description:           "Deferential whenever the owner may be using the computer.",
				Grace:                 d(15 * time.Second),
				GraceUnderPressure:    d(5 * time.Second),
				Cooldown:              d(5 * time.Minute),
				ExternalUtilSoftPct:   25,
				SoftWindow:            d(10 * time.Second),
				ExternalVRAMSoftMiB:   2048,
				DrainOnGameProcess:    true,
				DrainOnAmbiguous:      true,
				DrainOnSoftContention: true,
				GameConfirmUtilPct:    10,
				GameConfirmVRAMMiB:    512,
			},
			"school_hours": {
				Description:           "Weekday school hours: tolerant of ambiguous signals; real contention still wins.",
				Grace:                 d(60 * time.Second),
				GraceUnderPressure:    d(15 * time.Second),
				Cooldown:              d(2 * time.Minute),
				ExternalUtilSoftPct:   40,
				SoftWindow:            d(20 * time.Second),
				ExternalVRAMSoftMiB:   2560,
				DrainOnGameProcess:    true,
				DrainOnAmbiguous:      false,
				DrainOnSoftContention: true,
				GameConfirmUtilPct:    15,
				GameConfirmVRAMMiB:    768,
			},
			"ai_priority": {
				Description:           "Manual AI Priority: permissive, but never overrides safety, critical contention, or confirmed gaming.",
				Grace:                 d(120 * time.Second),
				GraceUnderPressure:    d(20 * time.Second),
				Cooldown:              d(time.Minute),
				ExternalUtilSoftPct:   50,
				SoftWindow:            d(30 * time.Second),
				ExternalVRAMSoftMiB:   2560,
				DrainOnGameProcess:    false,
				DrainOnAmbiguous:      false,
				DrainOnSoftContention: false,
				GameConfirmUtilPct:    20,
				GameConfirmVRAMMiB:    1024,
			},
		},
		DefaultProfile:    "normal",
		AIPriorityProfile: "ai_priority",
		Schedules: []Schedule{{
			Name:    "school-hours",
			Profile: "school_hours",
			Days:    []string{"mon", "tue", "wed", "thu", "fri"},
			Start:   "07:00",
			End:     "17:00",
			Enabled: true,
		}},
		Timezone:     "America/Chicago",
		Applications: DefaultApplications(),
		AntiThrash: AntiThrash{
			Window:         d(10 * time.Minute),
			MaxPreemptions: 2,
			SuppressFor:    d(30 * time.Minute),
		},
		Recovery: Recovery{
			StartupCooldown:           d(2 * time.Minute),
			ResumeCooldown:            d(3 * time.Minute),
			CrashBackoffInitial:       d(30 * time.Second),
			CrashBackoffMax:           d(30 * time.Minute),
			TelemetryRecoveryCooldown: d(2 * time.Minute),
			DeviceLostCooldown:        d(5 * time.Minute),
			CrashResetAfter:           d(30 * time.Minute),
		},
		Modes: Modes{
			PauseDurations:      []Duration{d(30 * time.Minute), d(time.Hour), d(2 * time.Hour), d(4 * time.Hour)},
			AIPriorityDurations: []Duration{d(30 * time.Minute), d(time.Hour), d(2 * time.Hour)},
			MaxAIPriority:       d(12 * time.Hour),
		},
		Signals: Signals{
			SessionStaleAfter: d(15 * time.Second),
			ProcessInterval:   d(2 * time.Second),
		},
		Security: Security{
			LoopbackTrust:         true,
			ManagementTokenFile:   `secrets\management.token`,
			InferenceTokenFile:    `secrets\inference.token`,
			AgentTokenFile:        `secrets\agent.token`,
			RequireInferenceToken: false,
		},
		Retention: Retention{EventsDays: 30, EventsMax: 50000, LogMaxMB: 20, LogFiles: 5},
		Metrics:   Metrics{Enabled: true},
		Logging:   Logging{Level: "info"},
	}
}

// DefaultApplications is the shipped classification list. Rules are
// evaluated in order and the first match wins, so launcher executables are
// listed before the library-folder rules that would otherwise catch them.
func DefaultApplications() []AppRule {
	return []AppRule{
		// Launchers: presence alone never makes the worker yield.
		{Name: "Steam", Exe: "steam.exe", Class: ClassLauncher},
		{Name: "Steam web helper", Exe: "steamwebhelper.exe", Class: ClassLauncher},
		{Name: "Epic Games Launcher", Exe: "EpicGamesLauncher.exe", Class: ClassLauncher},
		{Name: "Epic web helper", Exe: "EpicWebHelper.exe", Class: ClassLauncher},
		{Name: "Battle.net", Exe: "Battle.net.exe", Class: ClassLauncher},
		{Name: "EA app", Exe: "EADesktop.exe", Class: ClassLauncher},
		{Name: "Ubisoft Connect", Exe: "upc.exe", Class: ClassLauncher},
		{Name: "GOG Galaxy", Exe: "GalaxyClient.exe", Class: ClassLauncher},
		{Name: "Riot Client", Exe: "RiotClientServices.exe", Class: ClassLauncher},
		{Name: "Xbox app", Exe: "XboxPcApp.exe", Class: ClassLauncher},
		// Redistributable installers live in the library but are not games.
		{Name: "Steam redistributables", Glob: `*\steamapps\common\Steamworks Shared\*`, Class: ClassIgnore},
		// Anything else in a library folder is a game.
		{Name: "Steam library", Glob: `*\steamapps\common\*`, Class: ClassGame},
		{Name: "Epic library", PathPrefix: `C:\Program Files\Epic Games\`, Class: ClassGame},
		{Name: "Xbox / Game Pass", PathPrefix: `C:\XboxGames\`, Class: ClassGame},
		// Non-game GPU users: counted as external demand, never as gaming.
		{Name: "OBS Studio", Exe: "obs64.exe", Class: ClassOrdinary},
		{Name: "Discord", Exe: "Discord.exe", Class: ClassOrdinary},
		{Name: "Chrome", Exe: "chrome.exe", Class: ClassOrdinary},
		{Name: "Edge", Exe: "msedge.exe", Class: ClassOrdinary},
		{Name: "Firefox", Exe: "firefox.exe", Class: ClassOrdinary},
		// Desktop plumbing: never a trigger on its own.
		{Name: "Desktop Window Manager", Exe: "dwm.exe", Class: ClassIgnore},
		{Name: "Explorer", Exe: "explorer.exe", Class: ClassIgnore},
		{Name: "Shell", Exe: "ShellExperienceHost.exe", Class: ClassIgnore},
		{Name: "Start menu", Exe: "StartMenuExperienceHost.exe", Class: ClassIgnore},
		{Name: "Search", Exe: "SearchHost.exe", Class: ClassIgnore},
		{Name: "Text input", Exe: "TextInputHost.exe", Class: ClassIgnore},
		{Name: "Widgets", Exe: "Widgets.exe", Class: ClassIgnore},
	}
}
