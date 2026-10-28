// Dashboard: polls GET api/v1/status every 2 seconds while visible.
import { h, replace, toneIcon, warnIcon } from "./dom.js";
import { get, put, post, signIn, modeTokenReason, ApiError } from "./api.js";
import { setBanner, clearBanner } from "./banners.js";
import { appliedText, ownerView, quickChoices } from "./owner.js";
import { adoptsOptions, defaultDuration, durationChoices, modeKey, followsServer, normalizeOptions } from "./modes.js";
import {
  clockTime, configNotice, countdown, describeDurationChoice, eventKind, KIND_LABELS,
  eventStateText, humanDuration, mib, pct, parseTime, profileSource, titleize,
  MODE_LABELS,
} from "./format.js";

const POLL_MS = 2000;

let els = null;
let pollTimer = null;
let tickTimer = null;
let skewMs = 0; // server clock minus browser clock
let last = null;
let running = false;
let announcedKey = null; // the status the screen reader last heard

const serverNow = () => Date.now() + skewMs;

export function mount(main) {
  running = true;
  announcedKey = null;
  els = {
    head: h("section", { class: "card head", "aria-label": "AI status" }, h("p", null, "Loading status...")),
    // The only live region for the status: its text changes when the
    // situation does, not on every poll or clock tick.
    announce: h("p", { class: "sr-only", role: "status", "aria-live": "polite" }),
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
    els.announce,
    els.controls,
    els.notices,
    h("details", { class: "troubleshoot" },
      h("summary", null, "Details for troubleshooting"),
      h("p", { class: "muted small" }, "Evidence, timers and maintenance actions. You do not need these to pause or resume AI."),
      h("div", { class: "grid" },
        card("Why", els.decision, "wide"),
        card("Timers", els.timers),
        card("Mode and profile", els.mode),
        card("Runtime", els.runtime),
        card("GPU", els.gpu, "wide"),
        card("Session agent", els.agent),
        card("Recent errors", els.errors),
        buildMaintenance())),
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
  const view = ownerView(st, new Date(serverNow()));
  patch(els.head, view, () => renderHead(view));
  announce(view);
  patch(els.notices, [st.config_source, st.config_problem, st.gpu.confidence], () => renderNotices(st));
  patch(els.mode, [st.mode, st.profile], () => renderMode(st));
  patch(els.runtime, [st.runtime, st.state], () => renderRuntime(st.runtime, st.state));
  patch(els.gpu, st.gpu, () => renderGPU(st.gpu));
  patch(els.decision, [st.decision, st.trigger, st.condition, st.state], () => renderDecision(st));
  patch(els.timers, [st.timers, st.condition, st.decision.reason], () => renderTimers(st));
  patch(els.agent, st.agent, () => renderAgent(st.agent));
  patch(els.errors, st.recent_errors || [], () => renderErrors(st.recent_errors || []));
  updateControls(st);
  tick();
}

function renderHead(v) {
  return [
    h("div", { class: "badge tone-" + v.severity }, toneIcon(v.severity), h("span", null, v.badge)),
    h("div", { class: "headtext" },
      h("p", { class: "summary" }, v.headline),
      v.lines.map((l) => h("p", { class: "detail" }, l)),
      v.action && h("p", { class: "detail" }, h("a", { href: v.action.href }, v.action.label))),
  ];
}

// announce tells screen readers about a change in the situation, once. The
// first status is not announced; the page already shows it.
function announce(v) {
  if (v.key === announcedKey) return;
  if (announcedKey !== null) els.announce.textContent = v.badge + ". " + v.headline;
  announcedKey = v.key;
}

function renderNotices(st) {
  const out = [];
  if (st.config_source && st.config_source !== "primary") {
    out.push(h("div", { class: "notice error", role: "alert" }, warnIcon(), h("span", null, configNotice(st))));
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
    h("p", null, h("a", { href: "#/config/applications" }, "Review running GPU users and application rules")),
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
    h("p", { class: "muted small" }, "Condition: " + (st.condition || "unknown") + ". Internal state: " + (st.state || "unknown") + "."),
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

export function renderErrors(errs) {
  if (!errs.length) return h("p", { class: "muted" }, "None.");
  return h("ul", { class: "errors" }, errs.slice().reverse().map((e) => {
    const kind = KIND_LABELS[eventKind(e)] || "Step";
    const change = eventStateText(e);
    return h("li", null, h("time", null, clockTime(parseTime(e.time), null, { seconds: true })), " ",
      h("span", { class: "ekind ekind-" + eventKind(e) }, kind), " ",
      h("code", null, e.type), change ? " " + change : "", " ", e.message || "");
  }));
}

// Controls

function buildControls() {
  const status = h("p", { class: "action-status", role: "status" });
  let busy = false;
  let quickKey = null;
  let quickOptions = null;
  let untilTomorrowAt = null;
  const quickRows = h("div", { class: "quick" });
  const normalBtn = h("button", { type: "button", class: "quick-btn", onclick: () => {
    if (normalBtn.getAttribute("aria-disabled") !== "true") quick("auto", "");
  } }, "Back to normal");

  // quick is the one-click path: the same PUT as the form below, the same
  // authentication. It ignores clicks while one is in flight rather than
  // disabling the buttons, so focus stays on the button that was pressed.
  async function quick(mode, duration) {
    if (busy) return;
    busy = true;
    status.textContent = "Applying...";
    try {
      const body = { mode, set_by: "ui" };
      if (mode !== "auto") body.duration = duration;
      await put("api/v1/mode", body, { reason: modeTokenReason(mode) });
      status.textContent = appliedText(mode, duration, untilTomorrowAt);
      poll();
    } catch (err) {
      status.textContent = "Mode change failed: " + err.message;
    } finally {
      busy = false;
    }
  }
  function quickGroup(mode, label) {
    const choices = quickChoices(mode, quickOptions, untilTomorrowAt, new Date(serverNow()));
    return h("div", { class: "quick-group", role: "group", "aria-label": label },
      h("p", { class: "quick-label" }, label),
      h("div", { class: "quick-buttons" },
        choices.map((c) => h("button", { type: "button", class: "quick-btn", onclick: () => quick(mode, c.duration) }, c.label))));
  }
  // renderQuick rebuilds the buttons only when the choices change (a saved
  // Manual modes tab, a new day), and never under the visitor's focus.
  function renderQuick(st) {
    const key = JSON.stringify([st.mode_options, new Date(serverNow()).toDateString()]);
    if (key === quickKey || quickRows.contains(document.activeElement)) return;
    quickKey = key;
    quickOptions = st.mode_options;
    replace(quickRows,
      quickGroup("pause", "Pause AI"),
      quickGroup("ai_priority", "AI first for"));
  }
  const radios = Object.entries(MODE_LABELS).map(([v, label]) =>
    h("label", { class: "seg-option" },
      h("input", { type: "radio", name: "mode", value: v, onchange: fillDurations }), h("span", null, label)));
  const durSel = h("select", { id: "mode-duration" });
  const durWrap = h("div", { class: "field" }, h("label", { for: "mode-duration" }, "For"), durSel);
  const apply = h("button", { type: "submit", class: "primary" }, "Apply mode");
  // baseline is the form as it last followed the service (or was applied).
  // The form is dirty only while it differs from that, so an abandoned or
  // reverted edit lets polling take over again.
  let baseline;
  let options = { pause: [], ai_priority: [] };
  let syncedMode = null;
  let haveOptions = false;
  const form = h("form", { class: "mode-form", onsubmit: onApply },
    h("fieldset", { class: "segmented" }, h("legend", null, "Mode"), radios),
    durWrap, apply);

  function selected() {
    const r = form.querySelector("input[name=mode]:checked");
    return r ? r.value : "auto";
  }
  const formState = () => {
    const m = selected();
    return m === "auto" ? m : m + "|" + durSel.value;
  };
  function fillDurations() {
    const m = selected();
    durWrap.hidden = m === "auto";
    const opts = durationChoices(m, options);
    // The seed fill runs before any options arrive; its value is not a choice.
    const prev = haveOptions ? durSel.value : null;
    replace(durSel, opts.map((v) => h("option", { value: v })));
    labelDurations();
    durSel.value = opts.includes(prev) ? prev : defaultDuration(m, options);
  }
  // labelDurations words each choice; only a pause's "until tomorrow" ends
  // at the time the service reports, so only it names a time.
  function labelDurations() {
    const tomorrow = selected() === "pause" ? untilTomorrowAt : null;
    for (const o of durSel.options) o.textContent = describeDurationChoice(o.value, tomorrow, new Date(serverNow()));
  }
  async function onApply(e) {
    e.preventDefault();
    const mode = selected();
    const body = { mode, set_by: "ui" };
    if (mode !== "auto") body.duration = durSel.value;
    apply.disabled = true;
    status.textContent = "Applying...";
    try {
      // Pause AI and Back to normal need no token from this PC; AI first does.
      await put("api/v1/mode", body, { reason: modeTokenReason(mode) });
      baseline = formState();
      status.textContent = appliedText(mode, body.duration, untilTomorrowAt);
      poll();
    } catch (err) {
      status.textContent = "Mode change failed: " + err.message;
    } finally {
      apply.disabled = false;
    }
  }
  fillDurations();
  baseline = formState();
  const el = h("section", { class: "card controls" },
    h("h2", null, "Pause or resume AI"),
    quickRows,
    h("div", { class: "quick-group" }, normalBtn),
    h("details", { class: "more-options" },
      h("summary", null, "Other durations"),
      form),
    status);
  // sync follows the service: the configured durations, and the active mode
  // when it changed (a tray click, a pause that ran out). It never touches a
  // form the visitor is part-way through, and only checks a radio, so focus
  // stays where it is.
  el.sync = (st) => {
    untilTomorrowAt = st.mode_options && st.mode_options.until_tomorrow_at;
    const dirty = formState() !== baseline;
    // Configured durations are not applied under an edit in progress: the
    // select would be rebuilt and the visitor's choice changed. They are
    // compared again on every poll, so they land once the form is clean.
    const next = normalizeOptions(st.mode_options);
    const optionsChanged = adoptsOptions(haveOptions, dirty, options, next);
    if (optionsChanged) options = next;
    const m = st.mode || {};
    const key = modeKey(m);
    if (followsServer(dirty, syncedMode, key)) {
      const r = form.querySelector("input[value=" + (MODE_LABELS[m.mode] ? m.mode : "auto") + "]");
      if (r) r.checked = true;
      syncedMode = key;
      fillDurations();
      baseline = formState();
    } else if (optionsChanged) {
      fillDurations();
      baseline = formState();
    }
    haveOptions = true;
    labelDurations();
    renderQuick(st);
    normalBtn.setAttribute("aria-disabled", m.mode && m.mode !== "auto" ? "false" : "true");
    normalBtn.classList.toggle("is-current", !(m.mode && m.mode !== "auto"));
  };
  return el;
}

// buildMaintenance holds the two actions that are for troubleshooting, with
// a sentence on when each helps.
function buildMaintenance() {
  const status = h("p", { class: "action-status", role: "status" });
  async function action(path, question, done) {
    if (!window.confirm(question)) return;
    status.textContent = "Sending...";
    try {
      const res = await post(path);
      // 200 with accepted false: no runtime was loaded, which is not a failure.
      status.textContent = res && res.accepted === false && res.message ? res.message : done;
      poll();
    } catch (err) {
      status.textContent = "Request failed: " + err.message;
    }
  }
  return h("section", { class: "card maintenance" }, h("h2", null, "Maintenance"),
    h("p", null, h("strong", null, "Drain now"), ": unload the AI model right away, after active requests finish. ",
      "Use it before something that needs the whole GPU. AI loads again when policy allows."),
    h("p", null, h("strong", null, "Reload runtime"), ": restart the AI runtime with the saved settings. ",
      "Use it when a change under Configuration needs a restart to take effect."),
    h("div", { class: "row-actions" },
      h("button", { type: "button", onclick: () => action("api/v1/drain",
        "Drain now? Active requests get their grace period, then the model unloads. It reloads when policy allows.",
        "Drain requested.") }, "Drain now"),
      h("button", { type: "button", onclick: () => action("api/v1/reload",
        "Reload the runtime? It drains first, then restarts llama-server with the current configuration.",
        "Reload requested.") }, "Reload runtime")),
    status);
}

function updateControls(st) {
  els.controls.sync(st);
}
