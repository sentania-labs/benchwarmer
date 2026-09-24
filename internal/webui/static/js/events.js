// Events page: GET api/v1/events, newest first, paged with before_id.
import { h, replace } from "./dom.js";
import { get, signIn, ApiError } from "./api.js";
import { clockTime, mib, pct, parseTime, titleize } from "./format.js";
import { evidenceTable } from "./dashboard.js";

const PAGE = 50;

// Mirrors internal/events Type constants.
const TYPES = [
  "service_started", "service_stopping", "state_changed", "decision_changed",
  "load_started", "load_completed", "load_failed",
  "drain_started", "drain_completed", "grace_shortened", "preempted", "runtime_stopped",
  "runtime_crashed", "kill_failed", "vram_released", "vram_release_timeout", "orphan_killed",
  "cooldown_started", "cooldown_reset", "suppression_started", "recovery_scheduled",
  "mode_changed", "mode_expired", "config_changed", "config_rejected", "config_recovered",
  "telemetry_lost", "telemetry_recovered", "telemetry_degraded",
  "power_suspend", "power_resume", "session_change", "agent_connected", "agent_disconnected", "device_lost",
  "requests_rejected", "request_force_closed",
];

let state = null;

export function mount(main) {
  const filter = h("select", { id: "event-type", onchange: () => load(true) },
    h("option", { value: "" }, "All types"),
    TYPES.map((t) => h("option", { value: t }, titleize(t))));
  const refresh = h("button", { type: "button", onclick: () => load(true) }, "Refresh");
  const list = h("ol", { class: "events", "aria-label": "Events, newest first" });
  const more = h("button", { type: "button", hidden: true, onclick: () => load(false) }, "Load older events");
  const status = h("p", { class: "muted", role: "status" });
  state = { filter, list, more, status, before: 0, loading: false };
  replace(main,
    h("h1", null, "Events"),
    h("div", { class: "toolbar" },
      h("div", { class: "field inline" }, h("label", { for: "event-type" }, "Type"), filter), refresh),
    status, list, more);
  load(true);
  return { title: "Events", unmount: () => { state = null; } };
}

async function load(reset) {
  const s = state;
  if (!s || s.loading) return;
  s.loading = true;
  if (reset) {
    s.before = 0;
    replace(s.list);
  }
  s.status.textContent = "Loading...";
  const q = new URLSearchParams({ limit: String(PAGE) });
  if (s.filter.value) q.set("type", s.filter.value);
  if (s.before) q.set("before_id", String(s.before));
  try {
    const res = await get("api/v1/events?" + q.toString(), { interactive: false });
    if (state !== s) return;
    const evs = res.events || [];
    const now = new Date();
    for (const e of evs) s.list.append(row(e, now));
    s.before = res.next_before_id || 0;
    s.more.hidden = !s.before;
    s.status.textContent = s.list.children.length ? "" : "No events.";
  } catch (e) {
    if (state !== s) return;
    if (e instanceof ApiError && e.status === 401) {
      replace(s.status, "Reading events needs the management token. ",
        h("button", { type: "button", onclick: async () => { if (await signIn()) load(true); } }, "Enter token"));
    } else {
      s.status.textContent = "Could not load events: " + e.message;
    }
  } finally {
    s.loading = false;
  }
}

function telemetryTable(t) {
  const rows = [
    ["Confidence", t.confidence],
    ["Total utilization", pct(t.total_util_pct)],
    ["Benchwarmer utilization", pct(t.own_util_pct)],
    ["External utilization", pct(t.external_util_pct)],
    ["VRAM used", mib(t.vram_used_mib)],
    ["VRAM free", mib(t.vram_free_mib)],
    ["Benchwarmer VRAM", mib(t.own_vram_mib)],
    ["External VRAM", mib(t.external_vram_mib)],
    t.temperature_c != null && ["Temperature", Math.round(t.temperature_c) + " \u00b0C"],
  ].filter(Boolean);
  return h("dl", { class: "kv" }, rows.map(([k, v]) => [h("dt", null, k), h("dd", null, v)]));
}

// dataValue renders RFC 3339 strings in Data as local clock times.
function dataValue(v) {
  if (typeof v === "string" && /^\d{4}-\d\d-\d\dT\d\d:\d\d/.test(v)) {
    const d = parseTime(v);
    if (d) return clockTime(d, null, { seconds: true });
  }
  return typeof v === "object" ? JSON.stringify(v) : String(v);
}

function row(e, now) {
  const t = parseTime(e.time);
  const change = e.prev_state || e.state
    ? (e.prev_state || "?") + " \u2192 " + (e.state || "?") : "";
  const sev = e.severity || "info";
  const details = [];
  if (e.evidence && e.evidence.length) details.push(h("h3", null, "Evidence"), evidenceTable(e.evidence));
  if (e.telemetry) details.push(h("h3", null, "Telemetry"), telemetryTable(e.telemetry));
  if (e.data && Object.keys(e.data).length) {
    details.push(h("h3", null, "Data"), h("dl", { class: "kv" },
      Object.entries(e.data).map(([k, v]) => [h("dt", null, k), h("dd", null, dataValue(v))])));
  }
  const meta = [
    e.condition && "Condition: " + e.condition,
    e.profile && "Profile: " + e.profile,
    e.mode && "Mode: " + e.mode,
    e.runtime_pid && "PID: " + e.runtime_pid,
    e.request_id && "Request: " + e.request_id,
    "Event #" + e.id,
  ].filter(Boolean).join(" \u00b7 ");
  details.push(h("p", { class: "muted small" }, meta));
  return h("li", { class: "event sev-" + sev },
    h("details", null,
      h("summary", null,
        h("time", { datetime: e.time }, t ? clockTime(t, now, { seconds: true }) : ""),
        h("span", { class: "etype" }, titleize(e.type)),
        change && h("span", { class: "change" }, change),
        e.rule && h("code", { class: "rule" }, e.rule),
        h("span", { class: "msg" }, e.message || "")),
      h("div", { class: "event-body" }, details)));
}
