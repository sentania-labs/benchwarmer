package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// v010Config renders a config as v0.1.0 wrote it: rules without ids, no
// seen list, and none of the rules added since.
func v010Config(t *testing.T, extra ...AppRule) []byte {
	t.Helper()
	c := Default()
	c.BuiltinRulesSeen = nil
	var apps []AppRule
	for _, r := range c.Applications {
		if r.Exe == "LockApp.exe" || r.Exe == "LogonUI.exe" {
			continue
		}
		r.ID = ""
		apps = append(apps, r)
	}
	c.Applications = append(apps, extra...)
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "builtin_rules_seen")
	b, _ = json.Marshal(m)
	return b
}

func ruleIndex(c Config, exe string) int {
	for i, r := range c.Applications {
		if r.Exe == exe {
			return i
		}
	}
	return -1
}

func TestShippedRuleIDsAreUnique(t *testing.T) {
	ids := DefaultRuleIDs()
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			t.Fatalf("empty or duplicate shipped rule id %q", id)
		}
		seen[id] = true
	}
}

func TestParseAddsShippedRulesToOldConfig(t *testing.T) {
	local := AppRule{Name: "My game", Exe: "mygame.exe", Class: ClassGame}
	c, err := Parse(v010Config(t, local))
	if err != nil {
		t.Fatal(err)
	}
	lock, logon := ruleIndex(c, "LockApp.exe"), ruleIndex(c, "LogonUI.exe")
	if lock < 0 || logon < 0 {
		t.Fatalf("LockApp/LogonUI not added: %d %d", lock, logon)
	}
	if c.Applications[lock].Class != ClassIgnore || c.Applications[lock].ID == "" {
		t.Fatalf("bad merged rule: %+v", c.Applications[lock])
	}
	// Same position as in the shipped list: right after Firefox.
	if ff := ruleIndex(c, "firefox.exe"); lock != ff+1 || logon != ff+2 {
		t.Fatalf("rules not placed after their predecessor: ff=%d lock=%d logon=%d", ff, lock, logon)
	}
	if n := len(c.Applications); n != len(DefaultApplications())+1 {
		t.Fatalf("want shipped rules plus one local, got %d", n)
	}
	if last := c.Applications[len(c.Applications)-1]; last.ID != "" || last.Exe != "mygame.exe" {
		t.Fatalf("local rule lost or given an id: %+v", last)
	}
	if !slices.Equal(c.BuiltinRulesSeen, DefaultRuleIDs()) {
		t.Fatalf("seen list not recorded: %v", c.BuiltinRulesSeen)
	}
}

func TestParseKeepsEditedShippedRule(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal(v010Config(t), &m); err != nil {
		t.Fatal(err)
	}
	for _, a := range m["applications"].([]any) {
		if r := a.(map[string]any); r["exe"] == "Discord.exe" {
			r["class"] = ClassGame
		}
	}
	b, _ := json.Marshal(m)
	c, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range c.Applications {
		if r.Exe == "Discord.exe" {
			n++
			if r.Class != ClassGame {
				t.Fatalf("edit overwritten: %+v", r)
			}
		}
	}
	if n != 1 {
		t.Fatalf("Discord rule duplicated or lost: %d", n)
	}
}

func TestDeletedShippedRuleStaysDeleted(t *testing.T) {
	c := Default()
	c.Applications = slices.DeleteFunc(c.Applications, func(r AppRule) bool { return r.Exe == "Widgets.exe" })
	b, err := Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if ruleIndex(got, "Widgets.exe") >= 0 {
		t.Fatal("deleted shipped rule came back")
	}
}

func TestFutureShippedRuleIsOfferedOnce(t *testing.T) {
	// A config that has seen every current rule but one: the pretend new
	// rule is added the first time, then stays deleted once removed.
	c := Default()
	newID := "exe:widgets.exe"
	c.Applications = slices.DeleteFunc(c.Applications, func(r AppRule) bool { return r.ID == newID })
	c.BuiltinRulesSeen = slices.DeleteFunc(c.BuiltinRulesSeen, func(id string) bool { return id == newID })
	if !mergeBuiltinRules(&c) || ruleIndex(c, "Widgets.exe") < 0 {
		t.Fatal("unseen shipped rule not added")
	}
	c.Applications = slices.DeleteFunc(c.Applications, func(r AppRule) bool { return r.ID == newID })
	if mergeBuiltinRules(&c) || ruleIndex(c, "Widgets.exe") >= 0 {
		t.Fatal("rule offered twice")
	}
}

func TestValidateRejectsDuplicateRuleID(t *testing.T) {
	c := Clone(Default())
	c.Applications[1].ID = c.Applications[0].ID
	if _, ok := fieldErrs(t, Validate(c))["applications[1].id"]; !ok {
		t.Fatal("duplicate id accepted")
	}
}

func TestStoreLoadPersistsMergedRules(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, v010Config(t), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewStore(p)
	r, err := s.Load()
	if err != nil || r.Source != "primary" {
		t.Fatalf("got %v %v", r.Source, err)
	}
	if ruleIndex(r.Config, "LockApp.exe") < 0 {
		t.Fatal("LockApp missing after load")
	}
	b, _ := os.ReadFile(p)
	onDisk, err := Parse(b)
	if err != nil || ruleIndex(onDisk, "LockApp.exe") < 0 || len(onDisk.BuiltinRulesSeen) == 0 {
		t.Fatalf("merge not written to config.json: %v", err)
	}
	lg, _ := os.ReadFile(filepath.Join(filepath.Dir(p), "config.last-good.json"))
	if string(lg) != string(b) {
		t.Fatal("last-good not in step with the rewritten primary")
	}
	// A second load changes nothing.
	if r2, _ := s.Load(); len(r2.Config.Applications) != len(r.Config.Applications) {
		t.Fatal("second load changed the rule list")
	}
}

func TestLocalOverrideAboveShippedRuleKeepsIDsUnique(t *testing.T) {
	// A post-merge file: the shipped steam.exe rule carries its id and a
	// local id-less steam.exe override sits above it.
	c := Default()
	local := AppRule{Name: "My Steam", Exe: "steam.exe", Class: ClassGame}
	c.Applications = append([]AppRule{local}, c.Applications...)
	b, err := Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(b)
	if err != nil {
		t.Fatalf("valid config failed to load: %v", err)
	}
	if got.Applications[0].ID != "" || got.Applications[1].ID != "exe:steam.exe" {
		t.Fatalf("ids moved: %q %q", got.Applications[0].ID, got.Applications[1].ID)
	}
}

func TestLegacyDuplicateOfShippedRuleAdoptedOnce(t *testing.T) {
	local := AppRule{Name: "My Steam", Exe: "steam.exe", Class: ClassGame}
	var m map[string]any
	if err := json.Unmarshal(v010Config(t, local), &m); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(mustJSON(t, m)); err != nil {
		t.Fatalf("legacy config with a duplicate matcher failed: %v", err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestLegacyRulesMatchedAcrossSeparatorsAndCase(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal(v010Config(t), &m); err != nil {
		t.Fatal(err)
	}
	for _, a := range m["applications"].([]any) {
		r := a.(map[string]any)
		switch r["name"] {
		case "Steam library":
			r["glob"] = "*/STEAMAPPS/common/*"
		case "Epic library":
			r["path_prefix"] = "c:/program files/epic games/"
		}
	}
	c, err := Parse(mustJSON(t, m))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(c.Applications); n != len(DefaultApplications()) {
		t.Fatalf("legacy rules re-added as shipped duplicates: %d rules", n)
	}
	for _, r := range c.Applications {
		if (r.Name == "Steam library" || r.Name == "Epic library") && r.ID == "" {
			t.Fatalf("rule not recognised as shipped: %+v", r)
		}
	}
}
