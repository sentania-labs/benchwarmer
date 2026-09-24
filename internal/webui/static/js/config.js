// Configuration pages: GET api/v1/config into a draft, edit it through
// structured forms or raw JSON, and PUT the whole object back. Values shown
// as "<redacted>" are left untouched so the server restores them.
import { h, replace, warnIcon } from "./dom.js";
import { get, put, post, signIn, ApiError } from "./api.js";
import { clockTime, titleize } from "./format.js";
import { field, fields, getPath, idFor, errorSlot } from "./fields.js";
import { SECTIONS, sectionFor, sectionDefs } from "./configdefs.js";

// Kept across page visits so unsaved edits survive a trip to the dashboard.
const st = {
  cfg: null, draft: null, source: "", section: "runtime",
  errors: new Map(), clientErrors: new Map(), impact: null, message: null, loading: false,
};
let els = null;

const clone = (o) => JSON.parse(JSON.stringify(o));
const dirty = () => st.draft && JSON.stringify(st.draft) !== JSON.stringify(st.cfg);

const ctx = {
  get draft() { return st.draft; },
  errors: st.errors,
  clientErrors: st.clientErrors,
  changed: () => syncSaveBar(),
};

export function mount(main, rest) {
  st.section = SECTIONS.some((s) => s.key === rest[0]) ? rest[0] : st.section;
  els = {
    top: h("div", { class: "config-top" }),
    nav: h("nav", { class: "subnav", "aria-label": "Configuration sections" }),
    body: h("div", { class: "config-body" }),
    bar: h("div", { class: "savebar" }),
  };
  replace(main, h("h1", null, "Configuration"), els.top, els.nav, els.body, els.bar);
  window.addEventListener("beforeunload", beforeUnload);
  if (!st.draft || !dirty()) load();
  else renderAll();
  return {
    title: "Configuration",
    update: (r) => switchSection(r[0]),
    unmount: () => {
      window.removeEventListener("beforeunload", beforeUnload);
      els = null;
    },
  };
}

function beforeUnload(e) {
  if (dirty()) {
    e.preventDefault();
    e.returnValue = "";
  }
}

async function load() {
  st.loading = true;
  if (els) replace(els.body, h("p", null, "Loading configuration..."));
  try {
    const res = await get("api/v1/config", { interactive: false });
    st.cfg = res.config;
    st.draft = clone(res.config);
    st.source = res.source || "";
    st.errors.clear();
    st.clientErrors.clear();
  } catch (e) {
    st.loading = false;
    if (!els) return;
    if (e instanceof ApiError && e.status === 401) {
      replace(els.body, h("p", null, "Reading the configuration needs the management token. ",
        h("button", { type: "button", onclick: async () => { if (await signIn()) load(); } }, "Enter token")));
    } else {
      replace(els.body, h("p", { class: "field-error" }, "Could not load the configuration: " + e.message),
        h("button", { type: "button", onclick: load }, "Retry"));
    }
    return;
  }
  st.loading = false;
  renderAll();
}

function renderAll() {
  if (!els || !st.draft) return;
  renderTop();
  renderNav();
  renderSection();
  syncSaveBar();
}

function renderTop() {
  const parts = [];
  if (st.source && st.source !== "primary") {
    parts.push(h("div", { class: "notice error", role: "alert" }, warnIcon(),
      h("span", null, "The service is running on its " + (st.source === "last_good" ? "last known good" : "default") +
        " configuration because the primary file could not be used. Saving writes a new primary file.")));
  }
  const all = [...st.clientErrors, ...st.errors];
  if (all.length) {
    parts.push(h("div", { class: "error-summary", role: "alert", tabindex: "-1", id: "error-summary" },
      h("h2", null, all.length === 1 ? "1 problem to fix" : all.length + " problems to fix"),
      h("ul", null, all.map(([path, msg]) => h("li", null,
        h("a", { href: "#/config/" + sectionFor(path), onclick: (e) => { e.preventDefault(); jumpTo(path); } }, path),
        ": " + msg)))));
  }
  if (st.impact) parts.push(renderImpact(st.impact));
  if (st.message) parts.push(h("p", { class: "field-error", role: "alert" }, st.message));
  replace(els.top, parts);
}

function renderImpact(im) {
  const changed = im.changed || [];
  const items = [];
  if (!changed.length) items.push(h("li", null, "No settings changed."));
  else items.push(h("li", null, "Changed: " + changed.join(", ") + "."));
  if (im.runtime_reload) {
    items.push(h("li", null, h("strong", null, "Runtime reload needed"),
      " for " + (im.reload_sections || ["runtime"]).join(", ") + ". ",
      h("button", { type: "button", onclick: reloadRuntime }, "Reload runtime now")));
  }
  if (im.service_restart) {
    items.push(h("li", null, h("strong", null, "Service restart needed"),
      " for " + (im.restart_sections || []).join(", ") +
      ". Restart the Benchwarmer service on the PC for these to take effect."));
  }
  if (changed.length && !im.runtime_reload && !im.service_restart) items.push(h("li", null, "Applied live, nothing to restart."));
  return h("div", { class: "notice ok", role: "status" },
    h("div", null, h("strong", null, "Saved at " + clockTime(st.impact.at) + "."), h("ul", null, items)));
}

async function reloadRuntime() {
  if (!window.confirm("Reload the runtime now? It drains first, then restarts llama-server.")) return;
  try {
    await post("api/v1/reload");
    st.message = null;
    st.impact = { ...st.impact, runtime_reload: false, changed: st.impact.changed };
    renderTop();
  } catch (e) {
    st.message = "Reload failed: " + e.message;
    renderTop();
  }
}

function renderNav() {
  replace(els.nav, h("ul", null, SECTIONS.map((s) => {
    const n = [...st.errors.keys(), ...st.clientErrors.keys()].filter((p) => sectionFor(p) === s.key).length;
    return h("li", null, h("a", {
      href: "#/config/" + s.key,
      "aria-current": s.key === st.section ? "page" : null,
    }, s.label, n ? h("span", { class: "count", "aria-label": n + " problems" }, String(n)) : null));
  })));
}

function switchSection(key) {
  if (!SECTIONS.some((s) => s.key === key)) key = "runtime";
  if (key === st.section) return;
  if (st.section === "json" && !applyJSON()) {
    history.replaceState(null, "", "#/config/json");
    return;
  }
  st.section = key;
  renderNav();
  renderSection();
}

function jumpTo(path) {
  const sec = sectionFor(path);
  if (sec !== st.section) {
    if (st.section === "json" && !applyJSON()) return;
    st.section = sec;
    history.replaceState(null, "", "#/config/" + sec);
    renderNav();
    renderSection();
  }
  let p = path;
  for (;;) {
    const el = document.getElementById(idFor(p)) ||
      document.querySelector("[data-path=\"" + CSS.escape(p) + "\"] input, [data-path=\"" + CSS.escape(p) + "\"] select");
    if (el) {
      el.focus();
      el.scrollIntoView({ block: "center" });
      return;
    }
    const cut = Math.max(p.lastIndexOf("."), p.lastIndexOf("["));
    if (cut <= 0) return;
    p = p.slice(0, cut);
  }
}

function renderSection() {
  if (!els) return;
  const sec = SECTIONS.find((s) => s.key === st.section);
  let content;
  switch (st.section) {
    case "profiles": content = profilesSection(); break;
    case "schedules": content = schedulesSection(); break;
    case "applications": content = applicationsSection(); break;
    case "json": content = jsonSection(); break;
    default: content = sectionDefs(st.section).map((g) => group(g));
  }
  replace(els.body, h("h2", null, sec.label), sec.intro && h("p", { class: "intro" }, sec.intro), content);
}

function group(g) {
  return h("fieldset", { class: "group" }, h("legend", null, g.title), g.note && h("p", { class: "help" }, g.note),
    h("div", { class: "fields" }, fields(ctx, g.fields)));
}

// Profiles

const PROFILE_NAME = /^[a-z0-9][a-z0-9_-]*$/;

function profilesSection() {
  const names = Object.keys(st.draft.profiles || {}).sort();
  const opts = names.map((n) => [n, titleize(n)]);
  const newName = h("input", { id: "new-profile", type: "text", spellcheck: "false", placeholder: "e.g. evenings", autocomplete: "off" });
  const newErr = h("p", { class: "field-error", hidden: true });
  const add = () => {
    const n = newName.value.trim();
    let msg = "";
    if (!PROFILE_NAME.test(n)) msg = "Use lowercase letters, digits, - and _.";
    else if (st.draft.profiles[n]) msg = "A profile with that name exists.";
    if (msg) {
      newErr.textContent = msg;
      newErr.hidden = false;
      return;
    }
    const base = st.draft.profiles[st.draft.default_profile] || st.draft.profiles[names[0]] || {};
    st.draft.profiles[n] = { ...clone(base), description: "" };
    ctx.changed();
    renderSection();
    const el = document.getElementById(idFor("profiles." + n + ".description"));
    if (el) el.focus();
  };
  return [
    h("fieldset", { class: "group" }, h("legend", null, "Which profile applies"),
      h("div", { class: "fields" },
        field(ctx, { path: "default_profile", label: "Default profile", type: "select", options: opts,
          help: "Applies whenever no schedule is active." }),
        field(ctx, { path: "ai_priority_profile", label: "AI Priority profile", type: "select", options: opts,
          help: "Applies while AI Priority mode is on. Safety limits, critical contention and confirmed gaming still win." }))),
    names.map((n) => {
      const used = [st.draft.default_profile === n && "default", st.draft.ai_priority_profile === n && "AI Priority",
        ...(st.draft.schedules || []).filter((s) => s.profile === n).map((s) => "schedule " + s.name)].filter(Boolean);
      return h("fieldset", { class: "group", dataset: { path: "profiles." + n } },
        h("legend", null, titleize(n), h("code", { class: "muted" }, " " + n)),
        h("p", { class: "help" }, used.length ? "Used by: " + used.join(", ") + "." : "Not used by anything yet."),
        errorSlot(ctx, "profiles." + n),
        h("div", { class: "fields" }, fields(ctx, sectionDefs("profile", "profiles." + n))),
        h("div", { class: "row-actions" }, h("button", {
          type: "button", class: "danger",
          onclick: () => {
            if (!window.confirm("Remove profile “" + n + "” from the draft?" + (used.length ? " It is used by " + used.join(", ") + "; saving will fail until those point elsewhere." : ""))) return;
            delete st.draft.profiles[n];
            ctx.changed();
            renderSection();
          },
        }, "Remove profile")));
    }),
    h("fieldset", { class: "group" }, h("legend", null, "Add a profile"),
      h("div", { class: "field" }, h("label", { for: "new-profile" }, "Name"), newName,
        h("p", { class: "help" }, "Starts as a copy of the default profile."), newErr),
      h("button", { type: "button", onclick: add }, "Add profile")),
  ];
}

// Schedules

const DAYS = [["mon", "Mon"], ["tue", "Tue"], ["wed", "Wed"], ["thu", "Thu"], ["fri", "Fri"], ["sat", "Sat"], ["sun", "Sun"]];

function zoneList() {
  let zones = [];
  try {
    zones = Intl.supportedValuesOf("timeZone");
  } catch {
    zones = ["America/Chicago", "America/New_York", "America/Denver", "America/Los_Angeles", "Europe/London", "UTC"];
  }
  return h("datalist", { id: "tz-list" }, zones.map((z) => h("option", { value: z })));
}

function schedulesSection() {
  const list = st.draft.schedules || (st.draft.schedules = []);
  const opts = Object.keys(st.draft.profiles || {}).sort().map((n) => [n, titleize(n)]);
  const structural = (fn, focusId) => {
    fn();
    clearErrorsUnder("schedules");
    ctx.changed();
    renderSection();
    const el = focusId && document.getElementById(focusId);
    if (el) el.focus();
  };
  return [
    h("fieldset", { class: "group" }, h("legend", null, "Time zone"),
      zoneList(),
      field(ctx, { path: "timezone", label: "Schedule time zone", list: "tz-list", placeholder: "America/Chicago",
        help: "IANA zone name. Schedules follow its daylight-saving rules. Times on this page elsewhere use your browser's zone." })),
    list.map((s, i) => {
      const p = "schedules[" + i + "]";
      const days = new Set(s.days || []);
      const dayBoxes = DAYS.map(([v, l]) => {
        const id = idFor(p + ".days." + v);
        return h("span", { class: "day" },
          h("input", { type: "checkbox", id, checked: days.has(v), onchange: (e) => {
            const cur = new Set(getPath(st.draft, p + ".days") || []);
            if (e.target.checked) cur.add(v); else cur.delete(v);
            s.days = DAYS.map(([d]) => d).filter((d) => cur.has(d));
            ctx.changed();
          } }),
          h("label", { for: id }, l));
      });
      return h("fieldset", { class: "group", dataset: { path: p } },
        h("legend", null, "Schedule " + (i + 1) + (s.name ? ": " + s.name : "")),
        errorSlot(ctx, p),
        h("div", { class: "fields" },
          field(ctx, { path: p + ".name", label: "Name" }),
          field(ctx, { path: p + ".profile", label: "Profile", type: "select", options: opts }),
          h("fieldset", { class: "field days", dataset: { path: p + ".days" } },
            h("legend", null, "Days"), h("div", { class: "day-row" }, dayBoxes), errorSlot(ctx, p + ".days")),
          field(ctx, { path: p + ".start", label: "Start", type: "time", help: "Local time in the schedule time zone." }),
          field(ctx, { path: p + ".end", label: "End", type: "time", help: "Exclusive: the schedule stops at this minute." }),
          field(ctx, { path: p + ".enabled", label: "Enabled", type: "bool" })),
        h("div", { class: "row-actions" },
          h("button", { type: "button", class: "danger", onclick: () => {
            if (window.confirm("Remove schedule “" + (s.name || i + 1) + "” from the draft?")) structural(() => list.splice(i, 1), "add-schedule");
          } }, "Remove schedule")));
    }),
    h("button", { type: "button", id: "add-schedule", onclick: () => structural(() => list.push({
      name: "", profile: st.draft.default_profile, days: ["mon", "tue", "wed", "thu", "fri"],
      start: "09:00", end: "17:00", enabled: true,
    }), idFor("schedules[" + list.length + "].name")) }, "Add schedule"),
  ];
}

// Applications

const MATCHERS = [["exe", "Executable name"], ["path", "Exact path"], ["path_prefix", "Path prefix"], ["glob", "Glob"]];
const CLASSES = [["game", "Game"], ["launcher", "Launcher"], ["ordinary", "Ordinary GPU user"], ["ignore", "Ignore"]];
const MATCHER_HELP = {
  exe: "File name only, e.g. game.exe.",
  path: "Full path to one executable.",
  path_prefix: "Every executable under this folder.",
  glob: "Full path where * matches anything (including folders) and ? one character.",
};

function matcherOf(r) {
  return MATCHERS.map(([k]) => k).find((k) => r[k]) || "exe";
}

function clearErrorsUnder(prefix) {
  for (const k of [...st.errors.keys()]) if (k.startsWith(prefix)) st.errors.delete(k);
  for (const k of [...st.clientErrors.keys()]) if (k.startsWith(prefix)) st.clientErrors.delete(k);
  renderTop();
  renderNav();
}

function applicationsSection() {
  const list = st.draft.applications || (st.draft.applications = []);
  const structural = (fn, focusId) => {
    fn();
    clearErrorsUnder("applications");
    ctx.changed();
    renderSection();
    const el = focusId && document.getElementById(focusId);
    if (el) el.focus();
  };
  const rows = list.map((r, i) => {
    const p = "applications[" + i + "]";
    const kind = matcherOf(r);
    const kindId = idFor(p + ".matcher");
    const valueField = field(ctx, { path: p + "." + kind, label: "Match", help: MATCHER_HELP[kind] });
    const kindSel = h("select", { id: kindId, onchange: (e) => {
      const v = r[kind] || "";
      for (const [k] of MATCHERS) delete r[k];
      r[e.target.value] = v;
      structural(() => {}, kindId);
    } }, MATCHERS.map(([k, l]) => h("option", { value: k, selected: k === kind }, l)));
    const move = (d, label, id) => h("button", {
      type: "button", id, "aria-label": label + " rule " + (i + 1), disabled: i + d < 0 || i + d >= list.length,
      onclick: () => structural(() => {
        const [x] = list.splice(i, 1);
        list.splice(i + d, 0, x);
      }, idFor("applications[" + (i + d) + "]") + (d < 0 ? "-up" : "-down")),
    }, label);
    return h("li", { class: "rule", dataset: { path: p } },
      h("div", { class: "rule-head" }, h("span", { class: "rule-num" }, "#" + (i + 1)),
        h("span", { class: "rule-actions" },
          move(-1, "Up", idFor(p) + "-up"), move(1, "Down", idFor(p) + "-down"),
          h("button", { type: "button", class: "danger", "aria-label": "Remove rule " + (i + 1),
            onclick: () => structural(() => list.splice(i, 1), "add-rule") }, "Remove"))),
      errorSlot(ctx, p),
      h("div", { class: "fields" },
        field(ctx, { path: p + ".name", label: "Name", placeholder: "optional" }),
        h("div", { class: "field" }, h("label", { for: kindId }, "Matcher type"), kindSel),
        valueField,
        field(ctx, { path: p + ".class", label: "Class", type: "select", options: CLASSES })));
  });
  return [
    h("div", { class: "notice info" }, h("span", null,
      "Rules are checked top to bottom and the first match wins. Put specific rules (a launcher's own executable) above broad ones (a whole library folder). Matching ignores case and treats / and \\ alike.")),
    h("ol", { class: "rules" }, rows),
    h("button", { type: "button", id: "add-rule", onclick: () => structural(() => list.push({ exe: "", class: "game" }),
      idFor("applications[" + list.length + "].exe")) }, "Add rule at the end"),
  ];
}

// Advanced JSON

function jsonSection() {
  const ta = h("textarea", { id: "config-json", class: "json", rows: 30, spellcheck: "false",
    "aria-describedby": "config-json-help config-json-err", value: JSON.stringify(st.draft, null, 2) });
  const err = h("p", { class: "field-error", id: "config-json-err", hidden: true });
  els.json = { ta, err };
  return [
    h("p", { class: "help", id: "config-json-help" },
      "The full configuration as the API sees it. Durations are Go duration strings (\"15s\", \"5m0s\"). Leave \"<redacted>\" values as they are to keep the stored secret. Changes here apply to the draft when you switch sections or save."),
    ta, err,
    h("div", { class: "row-actions" },
      h("button", { type: "button", onclick: () => { if (applyJSON()) err.textContent = ""; } }, "Apply to draft"),
      h("button", { type: "button", onclick: () => { ta.value = JSON.stringify(st.draft, null, 2); err.hidden = true; } }, "Reset from draft")),
  ];
}

function applyJSON() {
  if (st.section !== "json" || !els || !els.json) return true;
  const { ta, err } = els.json;
  try {
    const v = JSON.parse(ta.value);
    if (!v || typeof v !== "object" || Array.isArray(v)) throw new Error("the top level must be an object");
    st.draft = v;
    err.hidden = true;
    ctx.changed();
    return true;
  } catch (e) {
    err.textContent = "Not valid JSON: " + e.message;
    err.hidden = false;
    ta.focus();
    return false;
  }
}

// Save bar

function syncSaveBar() {
  if (!els) return;
  const d = dirty();
  replace(els.bar,
    h("span", { class: "savestate", role: "status" }, d ? "Unsaved changes" : "No unsaved changes"),
    h("button", { type: "button", disabled: !d, onclick: discard }, "Discard"),
    h("button", { type: "button", class: "primary", onclick: save }, "Save configuration"));
}

function discard() {
  if (!window.confirm("Discard all unsaved changes?")) return;
  st.draft = clone(st.cfg);
  st.errors.clear();
  st.clientErrors.clear();
  st.message = null;
  renderAll();
}

async function save() {
  if (!applyJSON()) return;
  st.message = null;
  if (st.clientErrors.size) {
    renderTop();
    renderNav();
    focusSummary();
    return;
  }
  try {
    const res = await put("api/v1/config", st.draft);
    st.cfg = res.config;
    st.draft = clone(res.config);
    st.source = res.source || st.source;
    st.errors.clear();
    st.impact = { ...(res.impact || { changed: [] }), at: new Date() };
  } catch (e) {
    st.impact = null;
    st.errors.clear();
    if (e instanceof ApiError && e.status === 400 && e.body.details && e.body.details.length) {
      for (const d of e.body.details) st.errors.set(d.field || "(config)", d.message);
    } else {
      st.message = "Save failed: " + e.message;
    }
  }
  renderAll();
  if (st.errors.size) focusSummary();
  else if (els) els.top.scrollIntoView({ block: "start" });
}

function focusSummary() {
  const s = document.getElementById("error-summary");
  if (s) {
    s.focus();
    s.scrollIntoView({ block: "start" });
  }
}
