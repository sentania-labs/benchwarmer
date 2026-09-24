package config

import (
	"encoding/json"
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
		{"LAN inference without TLS", func(c *Config) { c.Listen.Inference = "0.0.0.0:8480" }, "listen.inference_tls.enabled"},
		{"TLS without certificate", func(c *Config) { c.Listen.InferenceTLS.Enabled = true }, "listen.inference_tls.cert_file"},
		{"PEM without key", func(c *Config) {
			c.Listen.InferenceTLS = TLS{Enabled: true, CertFile: `C:\x\server.crt`}
		}, "listen.inference_tls.key_file"},
		{"PFX file", func(c *Config) {
			c.Listen.InferenceTLS = TLS{Enabled: true, CertFile: `tls\server.pfx`}
		}, "listen.inference_tls.cert_file"},
		{"legacy PFX password file", func(c *Config) {
			c.Listen.InferenceTLS = TLS{Enabled: true, StoreSubject: "ss8510", LegacyPFXPasswordFile: `tls\pfx.pass`}
		}, "listen.inference_tls.pfx_password_file"},
		{"file and store together", func(c *Config) {
			c.Listen.InferenceTLS = TLS{Enabled: true, CertFile: `tls\\a.crt`, KeyFile: `tls\\a.key`, StoreSubject: "ss8510"}
		}, "listen.inference_tls"},
		{"bad thumbprint", func(c *Config) {
			c.Listen.InferenceTLS = TLS{Enabled: true, StoreThumbprint: "abc"}
		}, "listen.inference_tls.store_thumbprint"},
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
	c.Listen.InferenceTLS = TLS{Enabled: true, SelfSigned: true}
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

func TestConfigFromBeforeRunAsAndTLSStillLoads(t *testing.T) {
	b, _ := Marshal(Default())
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	delete(m["runtime"].(map[string]any), "run_as")
	delete(m["listen"].(map[string]any), "inference_tls")
	old, _ := json.Marshal(m)
	c, err := Parse(old)
	if err != nil {
		t.Fatalf("config written by an earlier build rejected: %v", err)
	}
	if c.Runtime.RunAs != RunAsLocalService || c.Listen.InferenceTLS.Enabled {
		t.Fatalf("upgrade defaults: %+v %+v", c.Runtime.RunAs, c.Listen.InferenceTLS)
	}
}

// Files written before PFX support was removed carry an empty
// pfx_password_file; they must still load, and saving drops the key.
func TestLegacyEmptyPFXPasswordFileParses(t *testing.T) {
	b, err := Marshal(Default())
	if err != nil {
		t.Fatal(err)
	}
	old := strings.Replace(string(b), `"inference_tls": {`, `"inference_tls": {
      "pfx_password_file": "",`, 1)
	if old == string(b) {
		t.Fatal("test setup: inference_tls not found")
	}
	c, err := Parse([]byte(old))
	if err != nil {
		t.Fatalf("legacy file rejected: %v", err)
	}
	if err := Validate(c); err != nil {
		t.Fatalf("legacy file invalid: %v", err)
	}
	out, _ := Marshal(c)
	if strings.Contains(string(out), "pfx_password_file") {
		t.Fatal("empty legacy key written back")
	}
}
