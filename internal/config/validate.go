package config

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	// Embed the IANA database: Windows has no zoneinfo files, and schedule
	// evaluation must follow the configured zone's DST rules.
	_ "time/tzdata"
)

// FieldError is one validation failure.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ValidationError lists every problem found, so a UI can show them together.
type ValidationError struct {
	Errors []FieldError `json:"errors"`
}

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Errors))
	for i, f := range e.Errors {
		parts[i] = f.Field + ": " + f.Message
	}
	return "invalid configuration: " + strings.Join(parts, "; ")
}

type validator struct{ errs []FieldError }

func (v *validator) add(field, format string, a ...any) {
	v.errs = append(v.errs, FieldError{Field: field, Message: fmt.Sprintf(format, a...)})
}

func (v *validator) durRange(field string, d Duration, lo, hi time.Duration) {
	if d.D() < lo || d.D() > hi {
		v.add(field, "must be between %s and %s (got %s)", lo, hi, d.D())
	}
}

func (v *validator) intRange(field string, x, lo, hi int) {
	if x < lo || x > hi {
		v.add(field, "must be between %d and %d (got %d)", lo, hi, x)
	}
}

func (v *validator) pctRange(field string, x float64) {
	if x <= 0 || x > 100 {
		v.add(field, "must be greater than 0 and at most 100 (got %g)", x)
	}
}

// Validate checks c for unsafe or internally inconsistent values. The bounds
// here are schema limits, not tuning defaults.
func Validate(c Config) error {
	v := &validator{}
	if c.SchemaVersion != SchemaVersion {
		v.add("schema_version", "must be %d (got %d)", SchemaVersion, c.SchemaVersion)
	}
	validateRuntime(v, c.Runtime)
	validateListen(v, c)

	t := c.Telemetry
	v.durRange("telemetry.sample_interval", t.SampleInterval, 250*time.Millisecond, 10*time.Second)
	v.durRange("telemetry.loss_grace", t.LossGrace, t.SampleInterval.D(), 5*time.Minute)
	v.intRange("telemetry.degraded_margin_pct", t.DegradedMarginPct, 0, 90)

	s := c.Safety
	if s.GPUTempCriticalC < 60 || s.GPUTempCriticalC > 105 {
		v.add("safety.gpu_temp_critical_c", "must be between 60 and 105 (got %g)", s.GPUTempCriticalC)
	}
	if s.GPUTempResumeC >= s.GPUTempCriticalC || s.GPUTempResumeC < 30 {
		v.add("safety.gpu_temp_resume_c", "must be at least 30 and below gpu_temp_critical_c (got %g)", s.GPUTempResumeC)
	}

	k := c.Contention
	v.intRange("contention.vram_free_critical_mib", k.VRAMFreeCriticalMiB, 64, 8192)
	v.intRange("contention.external_vram_critical_mib", k.ExternalVRAMCriticalMiB, 256, 65536)
	v.pctRange("contention.external_util_critical_pct", k.ExternalUtilCriticalPct)
	v.durRange("contention.critical_window", k.CriticalWindow, 0, time.Minute)
	v.durRange("contention.clear_window", k.ClearWindow, time.Second, 30*time.Minute)

	if len(c.Profiles) == 0 {
		v.add("profiles", "at least one profile is required")
	}
	names := make([]string, 0, len(c.Profiles))
	for n := range c.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		validateProfile(v, "profiles."+n, c.Profiles[n], k)
	}
	if _, ok := c.Profiles[c.DefaultProfile]; !ok {
		v.add("default_profile", "profile %q does not exist", c.DefaultProfile)
	}
	if _, ok := c.Profiles[c.AIPriorityProfile]; !ok {
		v.add("ai_priority_profile", "profile %q does not exist", c.AIPriorityProfile)
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil || c.Timezone == "" || c.Timezone == "Local" {
		v.add("timezone", "must be an IANA zone name such as America/Chicago (got %q)", c.Timezone)
	}
	seen := map[string]bool{}
	for i, sc := range c.Schedules {
		validateSchedule(v, fmt.Sprintf("schedules[%d]", i), sc, c.Profiles)
		if seen[sc.Name] {
			v.add(fmt.Sprintf("schedules[%d].name", i), "duplicate schedule name %q", sc.Name)
		}
		seen[sc.Name] = true
	}
	for i, r := range c.Applications {
		validateAppRule(v, fmt.Sprintf("applications[%d]", i), r)
	}

	a := c.AntiThrash
	v.durRange("anti_thrash.window", a.Window, time.Minute, 24*time.Hour)
	v.intRange("anti_thrash.max_preemptions", a.MaxPreemptions, 1, 100)
	v.durRange("anti_thrash.suppress_for", a.SuppressFor, 0, 24*time.Hour)

	r := c.Recovery
	v.durRange("recovery.startup_cooldown", r.StartupCooldown, 0, time.Hour)
	v.durRange("recovery.resume_cooldown", r.ResumeCooldown, 0, time.Hour)
	v.durRange("recovery.crash_backoff_initial", r.CrashBackoffInitial, time.Second, time.Hour)
	v.durRange("recovery.crash_backoff_max", r.CrashBackoffMax, r.CrashBackoffInitial.D(), 24*time.Hour)
	v.durRange("recovery.telemetry_recovery_cooldown", r.TelemetryRecoveryCooldown, 0, time.Hour)
	v.durRange("recovery.device_lost_cooldown", r.DeviceLostCooldown, 0, 24*time.Hour)
	v.durRange("recovery.crash_reset_after", r.CrashResetAfter, time.Minute, 24*time.Hour)

	m := c.Modes
	v.durRange("modes.max_ai_priority", m.MaxAIPriority, time.Minute, 7*24*time.Hour)
	for i, d := range m.PauseDurations {
		v.durRange(fmt.Sprintf("modes.pause_durations[%d]", i), d, time.Minute, 30*24*time.Hour)
	}
	for i, d := range m.AIPriorityDurations {
		v.durRange(fmt.Sprintf("modes.ai_priority_durations[%d]", i), d, time.Minute, m.MaxAIPriority.D())
	}

	v.durRange("signals.session_stale_after", c.Signals.SessionStaleAfter, time.Second, 10*time.Minute)
	v.durRange("signals.process_interval", c.Signals.ProcessInterval, 250*time.Millisecond, time.Minute)

	sec := c.Security
	for field, p := range map[string]string{
		"security.management_token_file": sec.ManagementTokenFile,
		"security.inference_token_file":  sec.InferenceTokenFile,
		"security.agent_token_file":      sec.AgentTokenFile,
	} {
		if p == "" {
			v.add(field, "is required")
		}
	}

	ret := c.Retention
	v.intRange("retention.events_days", ret.EventsDays, 1, 3650)
	v.intRange("retention.events_max", ret.EventsMax, 100, 10_000_000)
	v.intRange("retention.log_max_mb", ret.LogMaxMB, 1, 1024)
	v.intRange("retention.log_files", ret.LogFiles, 1, 100)

	switch c.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		v.add("logging.level", "must be debug, info, warn, or error (got %q)", c.Logging.Level)
	}

	sort.SliceStable(v.errs, func(i, j int) bool { return v.errs[i].Field < v.errs[j].Field })
	if len(v.errs) > 0 {
		return &ValidationError{Errors: v.errs}
	}
	return nil
}

// Flags Benchwarmer sets itself; allowing them in runtime.args would let the
// config move the runtime off loopback or change what the policy measured.
var reservedArgs = map[string]bool{
	"-m": true, "--model": true, "--host": true, "--port": true,
	"-c": true, "--ctx-size": true, "-ngl": true, "--gpu-layers": true, "--n-gpu-layers": true,
}

func validateRuntime(v *validator, r Runtime) {
	if r.Executable == "" || !isAbs(r.Executable) {
		v.add("runtime.executable", "must be an absolute path")
	}
	if r.ModelPath == "" || !isAbs(r.ModelPath) {
		v.add("runtime.model_path", "must be an absolute path")
	}
	for i, a := range r.Args {
		if strings.Contains(a, Redacted) {
			v.add(fmt.Sprintf("runtime.args[%d]", i), "contains the %s placeholder; re-enter the secret value", Redacted)
		}
		name, _, _ := strings.Cut(a, "=")
		if reservedArgs[name] {
			v.add(fmt.Sprintf("runtime.args[%d]", i), "%s is set by Benchwarmer; use the dedicated runtime field", name)
		}
	}
	v.intRange("runtime.context_size", r.ContextSize, 256, 1<<20)
	v.intRange("runtime.gpu_layers", r.GPULayers, 0, 9999)
	if r.RunAs != RunAsLocalService && r.RunAs != RunAsService {
		v.add("runtime.run_as", "must be %q or %q (got %q)", RunAsLocalService, RunAsService, r.RunAs)
	}
	if ip := net.ParseIP(r.Host); ip == nil || !ip.IsLoopback() {
		v.add("runtime.host", "must be a loopback address; the runtime must not be reachable from other machines (got %q)", r.Host)
	}
	v.intRange("runtime.port", r.Port, 1024, 65535)
	v.durRange("runtime.load_timeout", r.LoadTimeout, 5*time.Second, 30*time.Minute)
	v.intRange("runtime.required_free_vram_mib", r.RequiredFreeVRAMMiB, 0, 1<<20)
	v.durRange("runtime.kill_verify_timeout", r.KillVerifyTimeout, time.Second, 2*time.Minute)
	v.durRange("runtime.vram_release_timeout", r.VRAMReleaseTimeout, time.Second, 5*time.Minute)
	v.intRange("runtime.vram_release_tolerance_mib", r.VRAMReleaseToleranceMiB, 0, 4096)
	v.durRange("runtime.max_request_duration", r.MaxRequestDuration, 5*time.Second, 24*time.Hour)
	v.intRange("runtime.diagnostics_tail_bytes", r.DiagnosticsTailBytes, 1024, 1<<20)
}

func validateListen(v *validator, c Config) {
	ports := map[string]string{}
	check := func(field, addr string) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			v.add(field, "must be host:port (got %q)", addr)
			return
		}
		if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
			v.add(field, "invalid port %q", port)
			return
		}
		if host != "" && net.ParseIP(host) == nil {
			v.add(field, "host must be an IP address or empty for all interfaces (got %q)", host)
		}
		if other, dup := ports[port]; dup {
			v.add(field, "port %s is already used by %s", port, other)
		}
		ports[port] = field
	}
	ports[strconv.Itoa(c.Runtime.Port)] = "runtime.port"
	check("listen.inference", c.Listen.Inference)
	check("listen.management", c.Listen.Management)
	t := c.Listen.InferenceTLS
	if host, _, err := net.SplitHostPort(c.Listen.Inference); err == nil && !isLoopbackHost(host) && !t.Enabled {
		v.add("listen.inference_tls.enabled", "a non-loopback inference listener must use TLS")
	}
	if t.Enabled {
		sources := 0
		for _, set := range []bool{t.CertFile != "", t.StoreThumbprint != "" || t.StoreSubject != ""} {
			if set {
				sources++
			}
		}
		switch {
		case sources > 1:
			v.add("listen.inference_tls", "set either a certificate file or a certificate store selection, not both")
		case t.StoreThumbprint != "" && t.StoreSubject != "":
			v.add("listen.inference_tls.store_subject", "set store_thumbprint or store_subject, not both")
		case t.StoreThumbprint != "" && !isHex40(strings.ReplaceAll(t.StoreThumbprint, " ", "")):
			v.add("listen.inference_tls.store_thumbprint", "must be the 40-character hex SHA-1 thumbprint")
		case sources == 1 && t.CertFile == "":
			// Store-backed: nothing more to check here.
		case t.CertFile == "" && !t.SelfSigned:
			v.add("listen.inference_tls.cert_file", "set a certificate file or store selection, or enable self_signed for testing")
		case t.CertFile != "" && isPFXPath(t.CertFile) && t.KeyFile != "":
			v.add("listen.inference_tls.key_file", "must be empty when cert_file is a PFX (the key is inside it)")
		case t.CertFile != "" && !isPFXPath(t.CertFile) && t.KeyFile == "":
			v.add("listen.inference_tls.key_file", "is required with a PEM certificate")
		}
		if t.PFXPasswordFile != "" && !isPFXPath(t.CertFile) {
			v.add("listen.inference_tls.pfx_password_file", "only applies to a .pfx or .p12 cert_file")
		}
	}
	// Non-loopback management needs a way to authenticate.
	if host, _, err := net.SplitHostPort(c.Listen.Management); err == nil && !isLoopbackHost(host) && c.Security.ManagementTokenFile == "" {
		v.add("listen.management", "a non-loopback management listener requires security.management_token_file")
	}
}

func isHex40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range strings.ToLower(s) {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func isPFXPath(p string) bool {
	l := strings.ToLower(p)
	return strings.HasSuffix(l, ".pfx") || strings.HasSuffix(l, ".p12")
}

func isLoopbackHost(h string) bool {
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func validateProfile(v *validator, f string, p Profile, k Contention) {
	v.durRange(f+".grace", p.Grace, 0, 10*time.Minute)
	v.durRange(f+".grace_under_pressure", p.GraceUnderPressure, 0, p.Grace.D())
	v.durRange(f+".cooldown", p.Cooldown, 0, 24*time.Hour)
	v.pctRange(f+".external_util_soft_pct", p.ExternalUtilSoftPct)
	if p.ExternalUtilSoftPct >= k.ExternalUtilCriticalPct {
		v.add(f+".external_util_soft_pct", "must be below contention.external_util_critical_pct (%g)", k.ExternalUtilCriticalPct)
	}
	v.durRange(f+".soft_window", p.SoftWindow, 0, 10*time.Minute)
	v.intRange(f+".external_vram_soft_mib", p.ExternalVRAMSoftMiB, 64, 65536)
	if p.ExternalVRAMSoftMiB >= k.ExternalVRAMCriticalMiB {
		v.add(f+".external_vram_soft_mib", "must be below contention.external_vram_critical_mib (%d)", k.ExternalVRAMCriticalMiB)
	}
	v.pctRange(f+".game_confirm_util_pct", p.GameConfirmUtilPct)
	v.intRange(f+".game_confirm_vram_mib", p.GameConfirmVRAMMiB, 1, 65536)
}

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// Weekday parses a schedule day name.
func Weekday(s string) (time.Weekday, bool) {
	d, ok := weekdays[strings.ToLower(s)]
	return d, ok
}

// ClockMinutes parses "HH:MM" into minutes after midnight.
func ClockMinutes(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, errors.New("must be HH:MM in 24-hour time")
	}
	return t.Hour()*60 + t.Minute(), nil
}

func validateSchedule(v *validator, f string, s Schedule, profiles map[string]Profile) {
	if s.Name == "" {
		v.add(f+".name", "is required")
	}
	if _, ok := profiles[s.Profile]; !ok {
		v.add(f+".profile", "profile %q does not exist", s.Profile)
	}
	if len(s.Days) == 0 {
		v.add(f+".days", "at least one day is required")
	}
	for i, d := range s.Days {
		if _, ok := Weekday(d); !ok {
			v.add(fmt.Sprintf("%s.days[%d]", f, i), "must be one of sun, mon, tue, wed, thu, fri, sat (got %q)", d)
		}
	}
	start, err1 := ClockMinutes(s.Start)
	if err1 != nil {
		v.add(f+".start", "%v", err1)
	}
	end, err2 := ClockMinutes(s.End)
	if err2 != nil {
		v.add(f+".end", "%v", err2)
	}
	if err1 == nil && err2 == nil && start == end {
		v.add(f+".end", "must differ from start")
	}
}

func validateAppRule(v *validator, f string, r AppRule) {
	n := 0
	for _, m := range []string{r.Exe, r.Path, r.PathPrefix, r.Glob} {
		if m != "" {
			n++
		}
	}
	if n != 1 {
		v.add(f, "exactly one of exe, path, path_prefix, glob must be set")
	}
	if r.Exe != "" && strings.ContainsAny(r.Exe, `/\`) {
		v.add(f+".exe", "must be a file name, not a path (use path or path_prefix)")
	}
	switch r.Class {
	case ClassGame, ClassLauncher, ClassIgnore, ClassOrdinary:
	default:
		v.add(f+".class", "must be game, launcher, ignore, or ordinary (got %q)", r.Class)
	}
}

// isAbs accepts Windows absolute paths on any OS, so a Windows config can be
// validated in Linux CI, plus native absolute paths for development.
func isAbs(p string) bool {
	if filepath.IsAbs(p) {
		return true
	}
	return len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') &&
		((p[0] >= 'A' && p[0] <= 'Z') || (p[0] >= 'a' && p[0] <= 'z'))
}
