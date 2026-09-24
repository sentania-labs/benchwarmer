package classify

import (
	"testing"

	"github.com/sentania-labs/benchwarmer/internal/config"
	"github.com/sentania-labs/benchwarmer/internal/signals"
)

func TestDefaultApplications(t *testing.T) {
	c := New(config.DefaultApplications())
	tests := []struct {
		name, exe, path string
		class, rule     string
	}{
		{"steam launcher", "steam.exe", `C:\Program Files (x86)\Steam\steam.exe`, config.ClassLauncher, "Steam"},
		{"steam launcher upper case", "STEAM.EXE", `C:\PROGRAM FILES (X86)\STEAM\STEAM.EXE`, config.ClassLauncher, "Steam"},
		{"steam game", "eldenring.exe", `D:\SteamLibrary\steamapps\common\ELDEN RING\Game\eldenring.exe`, config.ClassGame, "Steam library"},
		{"steam game forward slashes", "game.exe", `D:/SteamLibrary/steamapps/common/Foo/game.exe`, config.ClassGame, "Steam library"},
		{"steamworks redistributable ignored", "vc_redist.x64.exe",
			`C:\Program Files (x86)\Steam\steamapps\common\Steamworks Shared\_CommonRedist\vcredist\2022\vc_redist.x64.exe`,
			config.ClassIgnore, "Steam redistributables"},
		{"epic launcher", "EpicGamesLauncher.exe", `C:\Program Files (x86)\Epic Games\Launcher\Portal\Binaries\Win64\EpicGamesLauncher.exe`, config.ClassLauncher, "Epic Games Launcher"},
		{"epic launcher under library prefix still launcher", "EpicGamesLauncher.exe", `C:\Program Files\Epic Games\Launcher\EpicGamesLauncher.exe`, config.ClassLauncher, "Epic Games Launcher"},
		{"epic game", "FortniteClient-Win64-Shipping.exe", `C:\Program Files\Epic Games\Fortnite\FortniteGame\Binaries\Win64\FortniteClient-Win64-Shipping.exe`, config.ClassGame, "Epic library"},
		{"xbox game", "Game.exe", `C:\XboxGames\Starfield\Content\Starfield.exe`, config.ClassGame, "Xbox / Game Pass"},
		{"obs ordinary", "obs64.exe", `C:\Program Files\obs-studio\bin\64bit\obs64.exe`, config.ClassOrdinary, "OBS Studio"},
		{"dwm ignored", "dwm.exe", `C:\Windows\System32\dwm.exe`, config.ClassIgnore, "Desktop Window Manager"},
		{"unknown", "notepad.exe", `C:\Windows\System32\notepad.exe`, "", ""},
		{"no path: exe rule matches", "steam.exe", "", config.ClassLauncher, "Steam"},
		{"no path: path rules cannot match", "eldenring.exe", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			class, rule, ok := c.Classify(signals.Process{Name: tt.exe, Path: tt.path})
			if class != tt.class || rule != tt.rule || ok != (tt.class != "") {
				t.Fatalf("got (%q, %q, %v), want (%q, %q)", class, rule, ok, tt.class, tt.rule)
			}
		})
	}
}

func TestMatchers(t *testing.T) {
	rules := []config.AppRule{
		{Path: `C:\Games\Exact\exact.exe`, Class: config.ClassGame},
		{Name: "prefix", PathPrefix: `c:/games/prefixed/`, Class: config.ClassGame},
		{Name: "glob-q", Glob: `C:\Tools\tool?.exe`, Class: config.ClassOrdinary},
		{Name: "exe", Exe: "Thing.EXE", Class: config.ClassOrdinary},
	}
	c := New(rules)
	tests := []struct {
		name, path string
		rule       string
	}{
		{"exact", `c:/GAMES/exact/EXACT.exe`, `path:C:\Games\Exact\exact.exe`},
		{"exact is not prefix", `C:\Games\Exact\exact.exe.bak`, ""},
		{"prefix", `C:\Games\Prefixed\sub\x.exe`, "prefix"},
		{"prefix needs separator", `C:\Games\PrefixedOther\x.exe`, ""},
		{"glob ? one char", `C:\Tools\tool1.exe`, "glob-q"},
		{"glob ? not zero chars", `C:\Tools\tool.exe`, ""},
		{"glob ? not two chars", `C:\Tools\tool12.exe`, ""},
		{"exe base name from path", `D:\anywhere\thing.exe`, "exe"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, rule, _ := c.Match("", tt.path)
			if rule != tt.rule {
				t.Fatalf("rule %q, want %q", rule, tt.rule)
			}
		})
	}
}

func TestFirstMatchWins(t *testing.T) {
	c := New([]config.AppRule{
		{Name: "a", Glob: `*\x.exe`, Class: config.ClassIgnore},
		{Name: "b", Exe: "x.exe", Class: config.ClassGame},
	})
	if class, rule, _ := c.Match("x.exe", `C:\x.exe`); class != config.ClassIgnore || rule != "a" {
		t.Fatalf("got %s/%s", class, rule)
	}
	// Without a path the glob cannot match, so the exe rule wins.
	if class, rule, _ := c.Match("x.exe", ""); class != config.ClassGame || rule != "b" {
		t.Fatalf("got %s/%s", class, rule)
	}
}

func TestGlob(t *testing.T) {
	tests := []struct {
		p, s string
		want bool
	}{
		{"*", "", true},
		{"*", "a/b/c", true},
		{"a*c", "abc", true},
		{"a*c", "a/b/c", true},
		{"a*c", "ab", false},
		{"*/steamapps/common/*", "d:/lib/steamapps/common/game/x.exe", true},
		{"*/steamapps/common/*", "d:/lib/steamapps/commonx", false},
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		{"**a", "xxa", true},
		{"a*b*c", "a-b-b-c", true},
		{"a*b*c", "a-b-b-", false},
		{"é?", "éx", true},
	}
	for _, tt := range tests {
		if got := globMatch(tt.p, tt.s); got != tt.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tt.p, tt.s, got, tt.want)
		}
	}
}
