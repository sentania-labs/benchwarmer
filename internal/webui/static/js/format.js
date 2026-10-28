// Pure formatting helpers. No DOM access, no clock reads except where a
// caller passes "now", so they are easy to reason about and test.

const UNIT_MS = { ns: 1e-6, us: 1e-3, "µs": 1e-3, "μs": 1e-3, ms: 1, s: 1000, m: 60000, h: 3600000 };

// parseGoDuration turns a Go duration string ("1h30m", "15s", "500ms") into
// milliseconds, or null when it is not valid.
export function parseGoDuration(s) {
  if (typeof s !== "string") return null;
  s = s.trim();
  if (s === "0") return 0;
  let sign = 1;
  if (s[0] === "-" || s[0] === "+") {
    if (s[0] === "-") sign = -1;
    s = s.slice(1);
  }
  if (s === "") return null;
  const re = /(\d+(?:\.\d*)?|\.\d+)(ns|us|µs|μs|ms|s|m|h)/y;
  let total = 0;
  let pos = 0;
  while (pos < s.length) {
    re.lastIndex = pos;
    const m = re.exec(s);
    if (!m) return null;
    total += parseFloat(m[1]) * UNIT_MS[m[2]];
    pos = re.lastIndex;
  }
  return sign * total;
}

// formatGoDuration renders milliseconds the way Go's time.Duration.String
// does for whole milliseconds and above ("5m0s", "1h0m0s", "1.5s", "250ms").
export function formatGoDuration(ms) {
  if (!Number.isFinite(ms)) return "0s";
  ms = Math.round(ms);
  if (ms === 0) return "0s";
  const neg = ms < 0;
  if (neg) ms = -ms;
  let out;
  if (ms < 1000) {
    out = ms + "ms";
  } else {
    const h = Math.floor(ms / 3600000);
    const m = Math.floor((ms % 3600000) / 60000);
    const secMs = ms % 60000;
    let sec = String(Math.floor(secMs / 1000));
    const frac = secMs % 1000;
    if (frac) sec += "." + String(frac).padStart(3, "0").replace(/0+$/, "");
    out = "";
    if (h) out += h + "h";
    if (h || m) out += m + "m";
    out += sec + "s";
  }
  return (neg ? "-" : "") + out;
}

// Units offered by duration inputs, largest first.
export const DURATION_UNITS = [
  { key: "h", label: "hours", ms: 3600000 },
  { key: "min", label: "minutes", ms: 60000 },
  { key: "s", label: "seconds", ms: 1000 },
  { key: "ms", label: "milliseconds", ms: 1 },
];

// splitDuration picks the largest unit that represents ms exactly, for a
// friendly number-plus-unit input.
export function splitDuration(ms) {
  if (!ms) return { value: 0, unit: "s" };
  for (const u of DURATION_UNITS) {
    if (ms % u.ms === 0) return { value: ms / u.ms, unit: u.key };
  }
  return { value: ms, unit: "ms" };
}

export function unitMs(key) {
  const u = DURATION_UNITS.find((x) => x.key === key);
  return u ? u.ms : 1000;
}

// humanDuration renders milliseconds for reading: "45 s", "5 min",
// "1 h 30 min", "2 d 3 h".
export function humanDuration(ms) {
  if (ms == null || !Number.isFinite(ms)) return "";
  if (ms < 0) ms = 0;
  if (ms < 1000) return ms === 0 ? "0 s" : Math.round(ms) + " ms";
  let s = Math.round(ms / 1000);
  if (s < 60) return s + " s";
  const d = Math.floor(s / 86400);
  s -= d * 86400;
  const h = Math.floor(s / 3600);
  s -= h * 3600;
  const m = Math.floor(s / 60);
  s -= m * 60;
  const parts = [];
  if (d) parts.push(d + " d");
  if (h) parts.push(h + " h");
  if (m && !d) parts.push(m + " min");
  if (s && !d && !h) parts.push(s + " s");
  return parts.join(" ");
}

// countdown renders remaining milliseconds as "4:07" or "1:02:03".
export function countdown(ms) {
  if (ms <= 0) return "0:00";
  let s = Math.ceil(ms / 1000);
  const h = Math.floor(s / 3600);
  s -= h * 3600;
  const m = Math.floor(s / 60);
  s -= m * 60;
  const ss = String(s).padStart(2, "0");
  return h ? h + ":" + String(m).padStart(2, "0") + ":" + ss : m + ":" + ss;
}

// parseTime turns an API timestamp into a Date, or null for missing and Go
// zero times.
export function parseTime(s) {
  if (!s || typeof s !== "string" || s.startsWith("0001-01-01")) return null;
  const d = new Date(s);
  return Number.isNaN(d.getTime()) ? null : d;
}

function sameDay(a, b) {
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
}

// clockTime renders a Date in the browser's local zone as a clock time,
// adding the date when it is not today ("7:45 PM", "Thu, Sep 24, 7:45 PM").
export function clockTime(d, now, opts = {}) {
  if (!(d instanceof Date)) d = parseTime(d);
  if (!d) return "";
  now = now || new Date();
  const t = { hour: "numeric", minute: "2-digit" };
  if (opts.seconds) t.second = "2-digit";
  if (sameDay(d, now)) return d.toLocaleTimeString([], t);
  const o = { ...t, weekday: "short", month: "short", day: "numeric" };
  if (d.getFullYear() !== now.getFullYear()) o.year = "numeric";
  return d.toLocaleString([], o);
}

// mib renders a MiB count: "512 MiB", "12.4 GiB".
export function mib(n) {
  if (n == null || !Number.isFinite(n)) return "unknown";
  if (Math.abs(n) >= 1024) return (n / 1024).toFixed(1) + " GiB";
  return Math.round(n) + " MiB";
}

export function pct(n) {
  if (n == null || !Number.isFinite(n)) return "unknown";
  return (Math.round(n * 10) / 10) + "%";
}

// titleize turns "school-hours" or "school_hours" into "School hours".
export function titleize(s) {
  s = String(s || "").replace(/[-_]+/g, " ").trim();
  s = s.replace(/\bai\b/gi, "AI");
  return s ? s[0].toUpperCase() + s.slice(1) : s;
}

// profileSource explains why a profile is active.
export function profileSource(src) {
  if (!src || src === "default") return "Default profile";
  if (src === "ai_priority") return "AI Priority mode";
  if (src.startsWith("schedule:")) return titleize(src.slice(9)) + " schedule";
  return titleize(src);
}

export const MODE_LABELS = { auto: "Auto", pause: "Pause AI", ai_priority: "AI Priority" };

// STATE_LABELS are owner-readable names for the internal lifecycle states
// (internal/state). The raw state stays available in Details for diagnosis.
export const STATE_LABELS = {
  STOPPED: "Idle",
  COOLDOWN: "Waiting",
  LOADING: "Loading",
  READY: "Ready",
  BUSY: "Busy",
  DRAINING: "Draining",
  PREEMPTING: "Stopping",
  SUPPRESSED: "Held",
  DISABLED: "Paused",
  ERROR: "Problem",
};

// stateLabel names a lifecycle state in owner terms; an unlisted or missing
// value is still readable rather than blank.
export function stateLabel(s) {
  if (!s) return "unknown";
  return STATE_LABELS[s] || titleize(s);
}

// Event kinds: a state transition is not an error, but a critical or warning
// event is always a problem whatever its type. For the rest, KIND_BY_TYPE is
// the reference for internal/events.Type; severity is the fallback for a type
// this build does not know.
export const KIND_LABELS = { problem: "Problem", change: "State change", step: "Step" };

const KIND_BY_TYPE = {
  load_failed: "problem", runtime_crashed: "problem", kill_failed: "problem",
  vram_release_timeout: "problem", orphan_killed: "problem", config_rejected: "problem",
  telemetry_lost: "problem", device_lost: "problem", requests_rejected: "problem",
  request_force_closed: "problem", service_misconfigured: "problem",
  provisioning_problem: "problem", tls_certificate_problem: "problem",

  state_changed: "change", decision_changed: "change", mode_changed: "change",
  mode_expired: "change", cooldown_started: "change", cooldown_reset: "change",
  suppression_started: "change", recovery_scheduled: "change", session_change: "change",
  power_suspend: "change", power_resume: "change", config_changed: "change",
  config_recovered: "change", telemetry_recovered: "change", telemetry_degraded: "change",
  agent_connected: "change", agent_disconnected: "change",
  tls_certificate_reloaded: "change",

  service_started: "step", service_stopping: "step", load_started: "step",
  load_completed: "step", drain_started: "step", drain_completed: "step",
  grace_shortened: "step", preempted: "step", runtime_stopped: "step",
  vram_released: "step", power_suspend_stop: "step",
  tls_certificate_loaded: "step",
};

const KIND_BY_SEVERITY = {
  critical: "problem", warning: "problem", notice: "change", info: "step",
};

// TRANSITION_TYPES are the events built from a policy decision or a mode
// change. They inherit the decision's severity, so a warning on one (yielding
// to a game) is the product working, not a fault.
const TRANSITION_TYPES = new Set([
  "state_changed", "decision_changed", "mode_changed", "mode_expired",
  "cooldown_started", "cooldown_reset", "suppression_started", "drain_started", "drain_completed",
  "grace_shortened", "preempted",
]);

// eventKind classifies one event as "problem", "change" or "step". A critical
// or warning event is a problem, except that a transition is a problem only
// when critical or when a state_changed enters ERROR (other events carry the
// controller's current state, not one they entered).
export function eventKind(e) {
  const t = e && e.type;
  const sev = e && e.severity;
  if (t && TRANSITION_TYPES.has(t)) {
    if (sev === "critical" || (t === "state_changed" && e.state === "ERROR")) return "problem";
  } else if (sev === "critical" || sev === "warning") {
    return "problem";
  }
  if (t && KIND_BY_TYPE[t]) return KIND_BY_TYPE[t];
  if (sev && KIND_BY_SEVERITY[sev]) return KIND_BY_SEVERITY[sev];
  return "step";
}

// typeGroups orders the type filter: problems first, then state changes,
// then ordinary steps. Every known type is listed exactly once, and each
// group's kind matches what eventKind gives for its types.
export const TYPE_GROUPS = [
  { label: "Problems", kind: "problem", types: ["load_failed", "runtime_crashed", "kill_failed", "vram_release_timeout", "orphan_killed", "config_rejected", "telemetry_lost", "device_lost", "requests_rejected", "request_force_closed", "service_misconfigured", "provisioning_problem", "tls_certificate_problem"] },
  { label: "State changes", kind: "change", types: ["state_changed", "decision_changed", "mode_changed", "mode_expired", "cooldown_started", "cooldown_reset", "suppression_started", "recovery_scheduled", "session_change", "power_suspend", "power_resume", "config_changed", "config_recovered", "telemetry_recovered", "telemetry_degraded", "agent_connected", "agent_disconnected", "tls_certificate_reloaded"] },
  { label: "Steps", kind: "step", types: ["service_started", "service_stopping", "load_started", "load_completed", "drain_started", "drain_completed", "grace_shortened", "preempted", "runtime_stopped", "vram_released", "power_suspend_stop", "tls_certificate_loaded"] },
];

// transitionText renders prev -> next with the owner names, "\u2014" for a
// missing side.
export function transitionText(prev, next) {
  const fmt = (s) => (s ? stateLabel(s) : "\u2014");
  return fmt(prev) + " \u2192 " + fmt(next);
}

// eventStateText says what an event records about the service state. Only an
// event with a previous state is a transition; every other event is stamped
// with the controller's current state, which is shown as plain context.
export function eventStateText(e) {
  if (e.prev_state) return transitionText(e.prev_state, e.state);
  return e.state ? "Service state: " + stateLabel(e.state) : "";
}

// eventRawStateText is eventStateText with the raw state names.
export function eventRawStateText(e) {
  if (e.prev_state) return "Raw states: " + e.prev_state + " \u2192 " + (e.state || "\u2014");
  return e.state ? "Raw state: " + e.state : "";
}

// describeDurationChoice labels a PUT /mode duration value. Given the time
// "until_tomorrow" would really end, it names that time.
export function describeDurationChoice(v, untilTomorrowAt, now) {
  if (v === "") return "Indefinitely";
  if (v === "until_tomorrow") {
    const t = parseTime(untilTomorrowAt);
    return t ? "Until " + clockTime(t, now) : "Until tomorrow";
  }
  if (v === "until_reboot") return "Until reboot";
  const ms = parseGoDuration(v);
  return ms == null ? v : humanDuration(ms);
}

// configNotice says which config.json problem the service is working around.
// A file that was found and rejected is never described as missing.
export function configNotice(st) {
  const running = st.config_source === "last_good"
    ? "Running on the last known good configuration"
    : "Running on built-in defaults";
  if (st.config_problem) {
    return "config.json could not be used. " + running + ". Fix this and save: " + st.config_problem + ".";
  }
  if (st.config_source === "last_good") {
    return running + ", because config.json could not be used. Check the Events page for the reason, then fix and save the configuration.";
  }
  return running + ": no config.json existed, so the defaults were written. Review the configuration and save it.";
}
