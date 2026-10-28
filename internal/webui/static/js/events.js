// Events page: GET api/v1/events, newest first, paged with before_id.
import { h, replace } from "./dom.js";
import { get, signIn, ApiError } from "./api.js";
import {
  KIND_LABELS, TYPE_GROUPS, clockTime, eventKind, mib, pct, parseTime,
  eventRawStateText, eventStateText, titleize,
} from "./format.js";
import { evidenceTable } from "./dashboard.js";

const PAGE = 50;
const REFRESH_MS = 5000;
// One request never returns more than this: internal/api caps limit at
// maxEventsLimit = 1000, so a deeper list needs more pages.
const MAX_PAGE = 1000;

let state = null;

export function mount(main) {
  const filter = h("select", { id: "event-type", onchange: () => load(true) }, typeOptions());
  const refresh = h("button", { type: "button", onclick: () => load(true) }, "Refresh");
  const list = h("ol", { class: "events", "aria-label": "Events, newest first" });
  const more = h("button", { type: "button", hidden: true, onclick: () => load(false) }, "Load older events");
  const status = h("p", { class: "muted", role: "status" });
  state = {
    filter, list, more, status, before: 0, shown: null, failure: null, pending: null, loading: false, timer: null,
    rows: [], summaries: new Map(), // id -> summary element, for focus restore
  };
  replace(main,
    h("h1", null, "Events"),
    h("div", { class: "toolbar" },
      h("div", { class: "field inline" }, h("label", { for: "event-type" }, "Type"), filter), refresh),
    status, list, more);
  load(true);
  start();
  return { title: "Events", unmount, visibility: (v) => { if (v) { start(); load(true, true); } else stop(); } };
}

function unmount() {
  stop();
  state = null;
}

// start/stop tie the quiet auto-refresh to tab visibility: a hidden tab does
// no work, and a returning tab is refreshed at once.
function start() {
  if (!state || state.timer) return;
  state.timer = setInterval(() => load(true, true), REFRESH_MS);
}

function stop() {
  if (state && state.timer) clearInterval(state.timer);
  if (state) state.timer = null;
}

// typeOptions builds the grouped filter: problems first, then state changes,
// then ordinary steps, so the list is scanable instead of 40 flat entries.
export function typeOptions() {
  return [
    h("option", { value: "" }, "All types"),
    ...TYPE_GROUPS.map((g) => h("optgroup", { label: g.label },
      g.types.map((t) => h("option", { value: t }, titleize(t))))),
  ];
}

// focusedId is the event id whose summary holds keyboard focus, or null.
function focusedId(s) {
  const a = document.activeElement;
  if (!a) return null;
  for (const [id, el] of s.summaries) if (el === a) return id;
  return null;
}

// openIds lists the events whose details the reader had expanded.
function openIds(s) {
  const open = [];
  for (const r of s.rows) if (r.details.open) open.push(r.id);
  return open;
}

// fetchPages reads one or more pages so a deep list keeps every row the
// reader already has: one response holds at most MAX_PAGE events, and the
// API answers next_before_id whenever an older page exists.
async function fetchPages(filterValue, before, want) {
  const out = [];
  let cursor = before || 0;
  let nextBefore = 0;
  for (;;) {
    const limit = Math.min(MAX_PAGE, want - out.length);
    const q = new URLSearchParams({ limit: String(limit) });
    if (filterValue) q.set("type", filterValue);
    if (cursor) q.set("before_id", String(cursor));
    const res = await get("api/v1/events?" + q.toString(), { interactive: false });
    const evs = res.events || [];
    for (const e of evs) out.push(e);
    nextBefore = res.next_before_id || 0;
    if (out.length >= want || !nextBefore || !evs.length) return { events: out, next_before_id: nextBefore };
    cursor = nextBefore;
  }
}

// sameIds reports whether a fresh page holds exactly the rendered rows.
function sameIds(evs, rows) {
  return evs.length === rows.length && evs.every((e, i) => String(e.id) === rows[i].id);
}

async function load(reset, background) {
  const s = state;
  if (!s) return;
  if (s.loading) {
    if (!background) s.pending = { reset };
    return;
  }
  s.loading = true;
  // A different filter than the one the rows were fetched with starts over:
  // the old row count, expansions and focus belong to the old list.
  const filterValue = reset ? s.filter.value : s.shown || "";
  const fresh = reset && filterValue !== s.shown;
  const keep = reset && !fresh;
  // Only the first fill of a list announces: a polite live region that
  // repeats "Loading..." on every refresh is noise for a screen reader.
  if (fresh && !(background && s.failure)) s.status.textContent = "Loading...";
  // A refresh asks for at least the rows already on screen, so an automatic
  // refresh cannot shrink a list the reader has paged through; that depth can
  // exceed one response, so fetchPages reads the extra pages.
  const want = reset ? Math.max(PAGE, keep ? s.rows.length : 0) : PAGE;
  try {
    const res = await fetchPages(filterValue, reset ? 0 : s.before, want);
    if (state !== s) return;
    // Interaction is read after the awaits, immediately before the swap, so
    // what the reader did during the fetch is what gets restored.
    const focus = keep ? focusedId(s) : null;
    const opened = new Set(keep ? openIds(s) : []);
    const unchanged = background && keep && sameIds(res.events, s.rows);
    if (!unchanged) {
      // Rows are built off-DOM and swapped in whole, so a failed or slow fetch
      // leaves the current list, its scroll position and focus untouched.
      const rows = reset ? [] : s.rows;
      const summaries = reset ? new Map() : s.summaries;
      const items = [];
      for (const e of res.events) {
        const { li, details, summary } = row(e, new Date(), opened);
        items.push(li);
        rows.push({ id: String(e.id), details });
        summaries.set(String(e.id), summary);
      }
      if (reset) replace(s.list, items);
      else s.list.append(...items);
      s.rows = rows;
      s.summaries = summaries;
    }
    s.shown = filterValue;
    s.before = res.next_before_id || 0;
    s.more.hidden = !s.before;
    s.failure = null;
    s.status.textContent = s.rows.length ? "" : "No events.";
    if (!unchanged && focus && s.summaries.has(focus)) s.summaries.get(focus).focus({ preventScroll: true });
  } catch (e) {
    if (state !== s) return;
    const failure = e instanceof ApiError && e.status === 401 ? "unauthorized" : "failed: " + e.message;
    const repeat = background && s.failure === failure;
    s.failure = failure;
    if (repeat) return;
    if (e instanceof ApiError && e.status === 401) {
      replace(s.status, "Reading events needs the management token. ",
        h("button", { type: "button", onclick: async () => { if (await signIn()) load(true); } }, "Enter token"));
    } else {
      s.status.textContent = "Could not load events: " + e.message;
    }
  } finally {
    s.loading = false;
    const next = s.pending;
    s.pending = null;
    if (next && state === s) load(next.reset);
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

function kindBadge(kind) {
  return h("span", { class: "ekind ekind-" + kind }, KIND_LABELS[kind] || kind);
}

// row renders one event. `opened` holds the ids whose details stay expanded.
export function row(e, now, opened) {
  const t = parseTime(e.time);
  const kind = eventKind(e);
  const stateText = eventStateText(e);
  const body = [];
  if (e.evidence && e.evidence.length) body.push(h("h3", null, "Evidence"), evidenceTable(e.evidence));
  if (e.telemetry) body.push(h("h3", null, "Telemetry"), telemetryTable(e.telemetry));
  if (e.data && Object.keys(e.data).length) {
    body.push(h("h3", null, "Data"), h("dl", { class: "kv" },
      Object.entries(e.data).map(([k, v]) => [h("dt", null, k), h("dd", null, dataValue(v))])));
  }
  // Raw values are kept beside the readable ones, so details still carry the
  // diagnostic data.
  const meta = [
    e.condition && "Condition: " + e.condition,
    e.profile && "Profile: " + e.profile,
    e.mode && "Mode: " + e.mode,
    e.runtime_pid && "PID: " + e.runtime_pid,
    e.request_id && "Request: " + e.request_id,
    "Event #" + e.id,
    e.type && "Type: " + e.type,
    eventRawStateText(e),
  ].filter(Boolean).join(" \u00b7 ");
  body.push(h("p", { class: "muted small" }, meta));
  const summary = h("summary", { tabindex: "0" },
    h("time", { datetime: e.time }, t ? clockTime(t, now, { seconds: true }) : ""),
    kindBadge(kind),
    h("span", { class: "etype" }, titleize(e.type)),
    stateText && h("span", { class: "change" }, stateText),
    e.rule && h("code", { class: "erule" }, e.rule),
    h("span", { class: "msg" }, e.message || ""));
  const details = h("details", { open: opened && opened.has(String(e.id)) },
    summary, h("div", { class: "event-body" }, body));
  return { li: h("li", { class: "event sev-" + (e.severity || "info") }, details), details, summary };
}
