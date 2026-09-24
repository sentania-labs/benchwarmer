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

// describeDurationChoice labels a PUT /mode duration value.
export function describeDurationChoice(v) {
  if (v === "") return "Indefinitely";
  if (v === "until_tomorrow") return "Until tomorrow";
  if (v === "until_reboot") return "Until reboot";
  const ms = parseGoDuration(v);
  return ms == null ? v : humanDuration(ms);
}
