// Dashboard: polls GET api/v1/status every 2 seconds while visible.
import { h, replace, conditionIcon, warnIcon } from "./dom.js";
import { get, put, post, signIn, ApiError } from "./api.js";
import { setBanner, clearBanner } from "./banners.js";
import {
  clockTime, countdown, describeDurationChoice, humanDuration, mib, pct, parseTime,
  profileSource, titleize, MODE_LABELS,
} from "./format.js";

const POLL_MS = 2000;
const DURATIONS = ["30m0s", "1h0m0s", "2h0m0s", "4h0m0s", "until_tomorrow", "until_reboot"];

let els = null;
let pollTimer = null;
let tickTimer = null;
let skewMs = 0; // server clock minus browser clock
let last = null;
let controlsSeeded = false;
let running = false;

const serverNow = () => Date.now() + skewMs;

export function mount(main) {
  running = true;
  controlsSeeded = false;
  els = {
    head: h("section", { class: "card head", "aria-live": "polite", "aria-label": "Condition" }, h("p", null, "Loading status...")),
    notices: h("div", { class: "notices" }),
    controls: buildControls(),
    mode: h("div"),
    runtime: h("div"),
    gpu: h("div"),
    decision: h("div"),
    timers: h("div"),
    agent: h("div"),
    errors: h("div"),
  };
  replace(main,
    h("h1", { class: "sr-only" }, "Dashboard"),
    els.head,
    els.notices,
    h("div", { class: "grid" },
      card("Why", els.decision, "wide"),
      card("Timers", els.timers),
      card("Mode and profile", els.mode),
      els.controls,
      card("Runtime", els.runtime),
      card("GPU", els.gpu, "wide"),
      card("Session agent", els.agent),
      card("Recent errors", els.errors),
    ),
  );
  start();
  return { title: "Dashboard", unmount, visibility: (v) => (v ? start() : stop()) };
}

function unmount() {
  running = false;
  stop();
  clearBanner("status");
  els = null;
}

function start() {
  if (!running || document.hidden) return;
  stop();
  poll();
  tickTimer = setInterval(tick, 1000);
}

function stop() {
  clearTimeout(pollTimer);
  clearInterval(tickTimer);
  pollTimer = tickTimer = null;
}

async function poll() {
  clearTimeout(pollTimer);
  try {
    const st = await get("api/v1/status", { interactive: false });
    if (!els) return;
    const t = parseTime(st.time);
    if (t) skewMs = t.getTime() - Date.now();
    last = st;
    clearBanner("status");
    render(st);
  } catch (e) {
    if (!els) return;
    if (e instanceof ApiError && e.status === 401) {
      setBanner("status", "warn", "Reading status needs the management token here (loopback trust is off or this is a remote browser). ",
        h("button", { type: "button", onclick: async () => { if (await signIn("Enter the management token to view status.")) poll(); } }, "Enter token"));
    } else {
      const when = last ? " Last update " + clockTime(parseTime(last.time), null, { seconds: true }) + "." : "";
      setBanner("status", "error", "Cannot reach the Benchwarmer service: " + e.message + "." + when);
    }
  }
  if (running && !document.hidden) pollTimer = setTimeout(poll, POLL_MS);
}

function tick() {
  if (!els) return;
  const now = serverNow();
  for (const el of document.querySelectorAll("[data-deadline]")) {
    const d = Date.parse(el.dataset.deadline);
    el.textContent = countdown(d - now);
  }
}

function card(title, body, extra) {
  return h("section", { class: "card " + (extra || "") }, h("h2", null, title), body);
}

// patch re-renders el only when the data it shows has changed, so focus,
// selection, and screen-reader announcements are not disturbed every poll.
function patch(el, data, fn) {
  const key = JSON.stringify(data);
  if (el.dataset.key === key) return;
  el.dataset.key = key;
  replace(el, fn(data));
}

function when(t) {
  const d = parseTime(t);
  return d ? clockTime(d) : "";
}

function dl(rows) {
  return h("dl", { class: "kv" }, rows.filter(Boolean).map(([k, v]) => [h("dt", null, k), h("dd", null, v)]));
}

function render(st) {
  patch(els.head, [st.condition, st.summary, st.state], () => renderHead(st));
  patch(els.notices, [st.config_source, st.gpu.confidence], () => renderNotices(st));
  patch(els.mode, [st.mode, st.profile], () => renderMode(st));
  patch(els.runtime, [st.runtime, st.state], () => renderRuntime(st.runtime, st.state));
  patch(els.gpu, st.gpu, () => renderGPU(st.gpu));
  patch(els.decision, [st.decision, st.trigger], () => renderDecision(st));
  patch(els.timers, [st.timers, st.condition, st.decision.reason], () => renderTimers(st));
  patch(els.agent, st.agent, () => renderAgent(st.agent));
  patch(els.errors, st.recent_errors || [], () => renderErrors(st.recent_errors || []));
  updateControls(st);
  tick();
}

function renderHead(st) {
  const cond = st.condition || "Unavailable";
  return [
    h("div", { class: "badge cond-" + cond.toLowerCase() }, conditionIcon(cond), h("span", null, cond)),
    h("p", { class: "summary" }, st.summary || ""),
    h("p", { class: "muted small" }, "Internal state: " + (st.state || "unknown")),
  ];
}

function renderNotices(st) {
  const out = [];
  if (st.config_source && st.config_source !== "primary") {
    const what = st.config_source === "last_good"
      ? "the last known good configuration, because the primary config file could not be used"
      : "built-in defaults, because no usable config file was found";
    out.push(h("div", { class: "notice error", role: "alert" }, warnIcon(),
      h("span", null, "Running on " + what + ". Check Recent errors and the Events page, then fix and save the configuration.")));
  }
  const c = st.gpu && st.gpu.confidence;
  if (c === "degraded") {
    out.push(h("div", { class: "notice warn" }, warnIcon(),
      h("span", null, "GPU telemetry is degraded: per-process attribution is unavailable, so the Benchwarmer vs external split is estimated and soft thresholds are tightened.")));
  } else if (c === "none") {
    out.push(h("div", { class: "notice error", role: "alert" }, warnIcon(),
      h("span", null, "No GPU telemetry. Benchwarmer yields the GPU while it cannot measure it.")));
  }
  return out;
}

function renderMode(st) {
  const m = st.mode || {};
  const p = st.profile || {};
  let modeText = MODE_LABELS[m.mode] || titleize(m.mode);
  if (m.mode && m.mode !== "auto") modeText += m.until ? " until " + when(m.until) : " (no end time)";
  return dl([
    ["Mode", modeText],
    m.set_by && ["Set by", m.set_by + (m.set_at ? " at " + when(m.set_at) : "")],
    ["Profile", titleize(p.active) || "unknown"],
    ["Because", profileSource(p.source)],
    p.next_change && ["Next profile change", when(p.next_change)],
    p.timezone && ["Schedule time zone", p.timezone],
  ]);
}

function renderRuntime(r, state) {
  const age = r.active_requests > 0 && r.oldest_request_seconds
    ? " (oldest running " + humanDuration(r.oldest_request_seconds * 1000) + ")" : "";
  return dl([
    ["Model", r.model || "none"],
    ["Ready", r.ready ? "Yes" : "No" + (state ? " (" + state + ")" : "")],
    ["Active requests", String(r.active_requests || 0) + age],
    r.loaded_at && ["Loaded", when(r.loaded_at) + (r.last_load_seconds ? ", took " + humanDuration(r.last_load_seconds * 1000) : "")],
    !r.loaded_at && r.last_load_seconds && ["Last load took", humanDuration(r.last_load_seconds * 1000)],
    r.footprint_mib && ["VRAM footprint", mib(r.footprint_mib)],
    r.pid && ["Process ID", String(r.pid)],
    ["Crashes", String(r.crash_count || 0)],
  ]);
}

function bar(label, value, cls) {
  const v = Math.max(0, Math.min(100, Number(value) || 0));
  const fill = h("div", { class: "fill " + (cls || "") });
  fill.style.width = v + "%";
  return h("div", { class: "meter" },
    h("div", { class: "meter-label" }, h("span", null, label), h("span", null, pct(value))),
    h("div", { class: "track", role: "img", "aria-label": label + " " + pct(value) }, fill));
}

function vramBar(g) {
  const total = g.vram_total_mib || (g.vram_used_mib + g.vram_free_mib) || 0;
  const seg = (mibVal, cls) => {
    const s = h("div", { class: "seg " + cls });
    s.style.width = (total ? Math.max(0, (mibVal / total) * 100) : 0) + "%";
    return s;
  };
  const other = Math.max(0, (g.vram_used_mib || 0) - (g.own_vram_mib || 0) - (g.external_vram_mib || 0));
  const label = "VRAM: Benchwarmer " + mib(g.own_vram_mib) + ", external " + mib(g.external_vram_mib) + ", free " + mib(g.vram_free_mib);
  return h("div", { class: "meter" },
    h("div", { class: "meter-label" }, h("span", null, "VRAM"), h("span", null, mib(g.vram_used_mib) + " used of " + mib(total))),
    h("div", { class: "track stack", role: "img", "aria-label": label },
      seg(g.own_vram_mib || 0, "own"), seg(g.external_vram_mib || 0, "ext"), seg(other, "other")),
    h("ul", { class: "legend" },
      h("li", null, h("span", { class: "key own" }), "Benchwarmer " + mib(g.own_vram_mib)),
      h("li", null, h("span", { class: "key ext" }), "External " + mib(g.external_vram_mib)),
      other > 0 && h("li", null, h("span", { class: "key other" }), "Unattributed " + mib(other)),
      h("li", null, h("span", { class: "key free" }), "Free " + mib(g.vram_free_mib))));
}

function renderGPU(g) {
  const conf = g.confidence || "none";
  const top = g.top_external || [];
  return [
    g.adapter && h("p", { class: "muted small" }, g.adapter),
    h("div", { class: "meters" },
      bar("Total utilization", g.total_util_pct, "total"),
      bar("Benchwarmer", g.own_util_pct, "own"),
      bar("External", g.external_util_pct, "ext"),
      vramBar(g)),
    dl([
      ["Temperature", g.temperature_c != null ? Math.round(g.temperature_c) + " °C" : "unknown"],
      ["Telemetry confidence", h("span", { class: "conf conf-" + conf }, conf === "high" ? "High" : conf === "degraded" ? "Degraded" : "None")],
      g.sampled_at && ["Sampled", clockTime(parseTime(g.sampled_at), null, { seconds: true })],
    ]),
    top.length > 0 && h("div", { class: "table-wrap" }, h("table", null,
      h("caption", null, "Top external GPU users"),
      h("thead", null, h("tr", null, h("th", null, "Process"), h("th", null, "Class"), h("th", null, "GPU"), h("th", null, "VRAM"))),
      h("tbody", null, top.map((a) => h("tr", null,
        h("td", null, a.name + (a.foreground ? " (foreground)" : "")),
        h("td", null, a.class || "unclassified"),
        h("td", null, pct(a.gpu_util_pct)),
        h("td", null, mib(a.vram_mib))))))),
  ];
}

export function evidenceTable(ev, caption) {
  if (!ev || !ev.length) return h("p", { class: "muted" }, "No evidence recorded.");
  return h("div", { class: "table-wrap" }, h("table", null,
    caption && h("caption", null, caption),
    h("thead", null, h("tr", null, h("th", null, "Signal"), h("th", null, "Value"), h("th", null, "Threshold"), h("th", null, "Source"))),
    h("tbody", null, ev.map((e) => h("tr", null,
      h("td", null, e.name), h("td", null, e.value), h("td", null, e.threshold || ""), h("td", null, e.source || ""))))));
}

const ACTIONS = { run: "Run", hold: "Hold (do not load)", drain: "Drain", preempt: "Preempt (unload now)" };

function renderDecision(st) {
  const d = st.decision || {};
  return [
    st.trigger && h("p", { class: "trigger" }, h("strong", null, "Trigger: "), st.trigger),
    h("p", { class: "reason" }, d.reason || ""),
    dl([
      ["Winning rule", (d.rule || "none") + (d.tier ? " (tier " + d.tier + ")" : "")],
      ["Action", ACTIONS[d.action] || d.action || ""],
      d.severity && ["Severity", titleize(d.severity)],
      d.also_matched && d.also_matched.length && ["Also matched", d.also_matched.join(", ")],
    ]),
    evidenceTable(d.evidence, "Evidence"),
  ];
}

function timerRow(label, t) {
  const d = parseTime(t);
  if (!d) return null;
  return h("li", null, h("strong", null, label + ": "),
    h("span", { class: "countdown", dataset: { deadline: d.toISOString() } }, ""), " left, ends " + clockTime(d));
}

function renderTimers(st) {
  const t = st.timers || {};
  const rows = [
    timerRow("Grace", t.grace_until),
    timerRow("Cooldown", t.cooldown_until),
    timerRow("Suppression", t.suppressed_until),
    timerRow("Recovery", t.recovery_until),
  ].filter(Boolean);
  let next = null;
  if (st.condition !== "Available") {
    const at = parseTime(t.next_load_at);
    const why = t.next_load_reason || (st.decision && st.decision.reason) || "";
    if (at) {
      next = h("p", { class: "next-load" }, "Next load allowed at ", h("strong", null, clockTime(at)),
        why ? " because " + why : "", " (in ", h("span", { dataset: { deadline: at.toISOString() } }, ""), ").");
    } else {
      next = h("p", { class: "next-load" }, "No load time scheduled" + (why ? ": " + why : "."));
    }
  }
  return [
    next,
    rows.length ? h("ul", { class: "timers" }, rows) : h("p", { class: "muted" }, "No timers running."),
  ];
}

function renderAgent(a) {
  return dl([
    ["Connected", h("span", { class: a.connected ? "ok" : "bad" }, a.connected ? "Yes" : "No")],
    a.last_report && ["Last report", clockTime(parseTime(a.last_report), null, { seconds: true })],
    a.session_id && ["Session", String(a.session_id)],
    !a.connected && ["Effect", "Foreground, fullscreen and idle signals are unknown."],
  ]);
}

function renderErrors(errs) {
  if (!errs.length) return h("p", { class: "muted" }, "None.");
  return h("ul", { class: "errors" }, errs.slice().reverse().map((e) =>
    h("li", null, h("time", null, clockTime(parseTime(e.time), null, { seconds: true })), " ",
      h("code", null, e.type), " ", e.message)));
}

// Controls

function buildControls() {
  const status = h("p", { class: "action-status", role: "status" });
  const radios = Object.entries(MODE_LABELS).map(([v, label]) =>
    h("label", { class: "seg-option" },
      h("input", { type: "radio", name: "mode", value: v, onchange: () => fillDurations() }), h("span", null, label)));
  const durSel = h("select", { id: "mode-duration" });
  const durWrap = h("div", { class: "field" }, h("label", { for: "mode-duration" }, "For"), durSel);
  const apply = h("button", { type: "submit", class: "primary" }, "Apply mode");
  const form = h("form", { class: "mode-form", onsubmit: onApply },
    h("fieldset", { class: "segmented" }, h("legend", null, "Mode"), radios),
    durWrap, apply);

  function selected() {
    const r = form.querySelector("input[name=mode]:checked");
    return r ? r.value : "auto";
  }
  function fillDurations() {
    const m = selected();
    durWrap.hidden = m === "auto";
    const opts = m === "pause" ? [...DURATIONS, ""] : DURATIONS;
    const prev = durSel.value;
    replace(durSel, opts.map((v) => h("option", { value: v }, describeDurationChoice(v))));
    durSel.value = opts.includes(prev) ? prev : opts[1];
  }
  async function onApply(e) {
    e.preventDefault();
    const mode = selected();
    const body = { mode, set_by: "ui" };
    if (mode !== "auto") body.duration = durSel.value;
    apply.disabled = true;
    status.textContent = "Applying...";
    try {
      await put("api/v1/mode", body);
      status.textContent = MODE_LABELS[mode] + " applied" + (mode !== "auto" ? " (" + describeDurationChoice(body.duration).toLowerCase() + ")" : "") + ".";
      poll();
    } catch (err) {
      status.textContent = "Mode change failed: " + err.message;
    } finally {
      apply.disabled = false;
    }
  }
  async function action(path, question, done) {
    if (!window.confirm(question)) return;
    status.textContent = "Sending...";
    try {
      await post(path);
      status.textContent = done;
      poll();
    } catch (err) {
      status.textContent = "Request failed: " + err.message;
    }
  }
  fillDurations();
  const el = h("section", { class: "card controls" }, h("h2", null, "Controls"), form,
    h("div", { class: "row-actions" },
      h("button", { type: "button", onclick: () => action("api/v1/drain",
        "Drain now? Active requests get their grace period, then the model unloads. It reloads when policy allows.",
        "Drain requested.") }, "Drain now"),
      h("button", { type: "button", onclick: () => action("api/v1/reload",
        "Reload the runtime? It drains first, then restarts llama-server with the current configuration.",
        "Reload requested.") }, "Reload runtime")),
    status);
  el.seed = (mode) => {
    const r = form.querySelector("input[value=" + (MODE_LABELS[mode] ? mode : "auto") + "]");
    if (r) r.checked = true;
    fillDurations();
  };
  return el;
}

function updateControls(st) {
  if (!controlsSeeded && st.mode) {
    els.controls.seed(st.mode.mode);
    controlsSeeded = true;
  }
}
