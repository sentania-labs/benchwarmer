package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultIsValid(t *testing.T) {
	if err := Validate(Default()); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultRoundTrips(t *testing.T) {
	b, err := Marshal(Default())
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if c.Profiles["normal"].Grace.D() != 15*time.Second {
		t.Fatalf("grace lost in round trip: %v", c.Profiles["normal"].Grace)
	}
}

func fieldErrs(t *testing.T, err error) map[string]string {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError, got %v", err)
	}
	m := map[string]string{}
	for _, e := range ve.Errors {
		m[e.Field] = e.Message
	}
	return m
}

func TestValidationRejectsUnsafeValues(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*Config)
		field string
	}{
		{"runtime on LAN", func(c *Config) { c.Runtime.Host = "0.0.0.0" }, "runtime.host"},
		{"runtime on public IP", func(c *Config) { c.Runtime.Host = "192.168.1.5" }, "runtime.host"},
		{"reserved arg", func(c *Config) { c.Runtime.Args = []string{"--host", "0.0.0.0"} }, "runtime.args[0]"},
		{"reserved arg with =", func(c *Config) { c.Runtime.Args = []string{"--port=9999"} }, "runtime.args[0]"},
		{"relative exe", func(c *Config) { c.Runtime.Executable = "llama-server.exe" }, "runtime.executable"},
		{"port collision", func(c *Config) { c.Listen.Management = "127.0.0.1:8480" }, "listen.management"},
		{"runtime port collision", func(c *Config) { c.Listen.Inference = "0.0.0.0:18481" }, "listen.inference"},
		{"resume above critical", func(c *Config) { c.Safety.GPUTempResumeC = 95 }, "safety.gpu_temp_resume_c"},
		{"critical temp too high", func(c *Config) { c.Safety.GPUTempCriticalC = 120 }, "safety.gpu_temp_critical_c"},
		{"soft above critical util", func(c *Config) {
			p := c.Profiles["normal"]
			p.ExternalUtilSoftPct = 70
			c.Profiles["normal"] = p
		}, "profiles.normal.external_util_soft_pct"},
		{"pressure grace above grace", func(c *Config) {
			p := c.Profiles["normal"]
			p.GraceUnderPressure = Duration(time.Minute)
			c.Profiles["normal"] = p
		}, "profiles.normal.grace_under_pressure"},
		{"grace absurd", func(c *Config) {
			p := c.Profiles["normal"]
			p.Grace = Duration(time.Hour)
			c.Profiles["normal"] = p
		}, "profiles.normal.grace"},
		{"missing default profile", func(c *Config) { c.DefaultProfile = "nope" }, "default_profile"},
		{"schedule unknown profile", func(c *Config) { c.Schedules[0].Profile = "nope" }, "schedules[0].profile"},
		{"schedule bad day", func(c *Config) { c.Schedules[0].Days = []string{"monday"} }, "schedules[0].days[0]"},
		{"schedule bad time", func(c *Config) { c.Schedules[0].Start = "7am" }, "schedules[0].start"},
		{"fixed offset tz", func(c *Config) { c.Timezone = "Local" }, "timezone"},
		{"bogus tz", func(c *Config) { c.Timezone = "Mars/Olympus" }, "timezone"},
		{"rule with two matchers", func(c *Config) { c.Applications[0].Path = `C:\x.exe` }, "applications[0]"},
		{"rule bad class", func(c *Config) { c.Applications[0].Class = "boss" }, "applications[0].class"},
		{"exe rule with path", func(c *Config) { c.Applications[0].Exe = `C:\Games\x.exe` }, "applications[0].exe"},
		{"wrong schema version", func(c *Config) { c.SchemaVersion = 2 }, "schema_version"},
		{"backoff max below initial", func(c *Config) { c.Recovery.CrashBackoffMax = Duration(time.Second) }, "recovery.crash_backoff_max"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Clone(Default())
			tc.mut(&c)
			errs := fieldErrs(t, Validate(c))
			if _, ok := errs[tc.field]; !ok {
				t.Fatalf("expected error on %s, got %v", tc.field, errs)
			}
		})
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	b, _ := Marshal(Default())
	s := strings.Replace(string(b), `"schema_version": 1,`, `"schema_version": 1, "grace_seconds": 5,`, 1)
	if _, err := Parse([]byte(s)); err == nil || !strings.Contains(err.Error(), "grace_seconds") {
		t.Fatalf("want unknown field error, got %v", err)
	}
}

func TestStoreFirstRunWritesDefaults(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(filepath.Join(dir, "config.json"))
	r, err := s.Load()
	if err != nil || r.Source != "default" {
		t.Fatalf("got %v %v", r.Source, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.last-good.json")); err != nil {
		t.Fatal("last-good not written")
	}
}

func TestStoreRecoversFromCorruptPrimary(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	s := NewStore(p)
	c := Default()
	c.Profiles["normal"] = func() Profile { x := c.Profiles["normal"]; x.Grace = Duration(12 * time.Second); return x }()
	if err := s.Save(c); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"schema_version": 1, "runtime": {`), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if r.Source != "last_good" || r.PrimaryErr == nil {
		t.Fatalf("source=%s primaryErr=%v", r.Source, r.PrimaryErr)
	}
	if r.Config.Profiles["normal"].Grace.D() != 12*time.Second {
		t.Fatal("did not recover saved values")
	}
	// The broken primary is left for the operator to inspect.
	if b, _ := os.ReadFile(p); !strings.Contains(string(b), `"runtime": {`) {
		t.Fatal("broken primary was overwritten")
	}
}

func TestStoreSaveRejectsInvalidAndKeepsOld(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	s := NewStore(p)
	if err := s.Save(Default()); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(p)
	bad := Default()
	bad.Runtime.Host = "0.0.0.0"
	if err := s.Save(bad); err == nil {
		t.Fatal("invalid config saved")
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Fatal("file changed on rejected save")
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestRedactArgs(t *testing.T) {
	in := []string{"--api-key", "sk-123", "--api-key-file", `C:\k.txt`, "--token=abc", "-t", "8", "--hf-token", "hf_x"}
	got := RedactArgs(in)
	want := []string{"--api-key", Redacted, "--api-key-file", `C:\k.txt`, "--token=" + Redacted, "-t", "8", "--hf-token", Redacted}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v", got)
		}
	}
	if in[1] != "sk-123" {
		t.Fatal("input mutated")
	}
}

func TestRedactAndRestoreRoundTrip(t *testing.T) {
	c := Default()
	c.Runtime.Args = []string{"--api-key", "sk-123", "--threads", "8"}
	red := Redact(c)
	if strings.Contains(strings.Join(red.Runtime.Args, " "), "sk-123") {
		t.Fatal("secret leaked")
	}
	back := RestoreRedacted(red, c)
	if back.Runtime.Args[1] != "sk-123" {
		t.Fatalf("not restored: %v", back.Runtime.Args)
	}
}

func TestClassifyImpact(t *testing.T) {
	a := Default()
	b := Clone(a)
	b.Runtime.MaxRequestDuration = Duration(time.Minute)
	if im := Classify(a, b); im.RuntimeReload || im.ServiceRestart || len(im.Changed) != 1 {
		t.Fatalf("live runtime field: %+v", im)
	}
	b.Runtime.ContextSize = 4096
	if im := Classify(a, b); !im.RuntimeReload {
		t.Fatalf("context size needs reload: %+v", im)
	}
	c := Clone(a)
	c.Listen.Inference = "0.0.0.0:8480"
	if im := Classify(a, c); !im.ServiceRestart {
		t.Fatalf("listener needs restart: %+v", im)
	}
	d := Clone(a)
	d.Profiles["normal"] = func() Profile { x := d.Profiles["normal"]; x.Cooldown = Duration(time.Minute); return x }()
	if im := Classify(a, d); im.RuntimeReload || im.ServiceRestart {
		t.Fatalf("profile change is live: %+v", im)
	}
}

func TestRestoreRedactedSurvivesArgEdits(t *testing.T) {
	old := Default()
	old.Runtime.Args = []string{"--api-key", "sk-123", "--threads", "8"}
	in := Redact(old)
	in.Runtime.Args = append([]string{"--flash-attn"}, in.Runtime.Args...) // user adds an arg in front
	got := RestoreRedacted(in, old).Runtime.Args
	if strings.Join(got, " ") != "--flash-attn --api-key sk-123 --threads 8" {
		t.Fatalf("got %v", got)
	}
}
