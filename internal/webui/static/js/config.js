// Configuration pages: GET api/v1/config into a draft, edit it through
// structured forms or raw JSON, and PUT the whole object back. Values shown
// as "<redacted>" are left untouched so the server restores them.
import { proposedRule, sameMatcher, snapshotFresh, searchableValues } from "./applications.js";
import { waitForRestart } from "./restart.js";
import { compatibilityMessage } from "./compatibility.js";
import { h, replace, warnIcon } from "./dom.js";
import { get, put, post, signIn, ApiError } from "./api.js";
import { clockTime, titleize } from "./format.js";
import { field, fields, getPath, idFor, errorSlot, refreshErrors } from "./fields.js";
import { SECTIONS, sectionFor, sectionDefs } from "./configdefs.js";

// Kept across page visits so unsaved edits survive a trip to the dashboard.
const st = {
  cfg: null, draft: null, source: "", section: "runtime",
  errors: new Map(), clientErrors: new Map(), impact: null, message: null, loading: false, saving: false, setup: null,
};
let els = null;
let modelChoices = [];

const clone = (o) => JSON.parse(JSON.stringify(o));
const dirty = () => st.draft && (JSON.stringify(st.draft) !== JSON.stringify(st.cfg) || st.jsonText != null);

const ctx = {
  get draft() { return st.draft; },
  errors: st.errors,
  clientErrors: st.clientErrors,
  changed: () => { syncSaveBar(); updateCompatibility(); if (els) refreshProfilePreviews(st.draft, els.body); },
  modelPathChanged: () => updateModelChoices(),
};

export function mount(main, rest) {
  appView.query = "";
  appView.gpu = null;
  st.section = SECTIONS.some((s) => s.key === rest[0]) ? rest[0] : st.section;
  if (st.section !== "json") st.jsonText = null;
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
    loadChoices();
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

// loadChoices fills the suggestion lists behind the model, runtime, and
// certificate fields from GET /api/v1/setup. Failure only loses the
// suggestions; the fields still take typed values.
async function loadChoices() {
  let info;
  try {
    info = await get("api/v1/setup", { interactive: false });
  } catch {
    return;
  }
  st.setup = info;
  updateCompatibility();
  const mib = (b) => Math.round(b / 1048576).toLocaleString() + " MiB";
  modelChoices = (info.models || []).map((m) => [m.path, m.name + " (" + mib(m.size_bytes) + ")"]);
  updateModelChoices();
  const lists = {
    "dl-models": (info.models || []).map((m) => [m.path, m.name + " (" + mib(m.size_bytes) + ")"]),
    "dl-runtimes": (info.runtimes || []).map((r) => [r.path, r.name]),
    "dl-certs": (info.certificates || []).map((c) => [c.thumbprint, c.subject + ", expires " + new Date(c.not_after).toLocaleDateString()]),
    "dl-cert-names": [...new Set((info.certificates || []).flatMap((c) => c.dns_names || []))].map((n) => [n, n]),
  };
  for (const [id, opts] of Object.entries(lists)) {
    let dl = document.getElementById(id);
    if (!dl) {
      dl = h("datalist", { id });
      document.body.append(dl);
    }
    replace(dl, opts.map(([value, label]) => h("option", { value }, label)));
  }
}

function updateModelChoices() {
  const picker = document.querySelector('[data-model-picker]');
  if (!picker || document.activeElement === picker) return;
  const current = getPath(st.draft, "runtime.model_path") || "";
  const options = modelChoices.slice();
  if (!options.some(([v]) => v === current)) options.unshift([current, current ? current + " (custom)" : "Choose an installed model"]);
  replace(picker, options.map(([value, label]) => h("option", { value, selected: value === current }, label)));
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
      " for " + (im.restart_sections || []).join(", ") + ". ",
      h("button", { type: "button", onclick: restartService }, "Restart service now")));
  }
  if (im.note) items.push(h("li", null, im.note));
  if (changed.length && !im.runtime_reload && !im.service_restart && !im.note) items.push(h("li", null, "Applied live, nothing to restart."));
  return h("div", { class: "notice ok", role: "status" },
    h("div", null, h("strong", null, "Saved at " + clockTime(st.impact.at) + "."), h("ul", null, items)));
}

async function reloadRuntime() {
  if (!window.confirm("Reload the runtime now? It drains first, then restarts llama-server.")) return;
  try {
    const res = await post("api/v1/reload");
    st.message = null;
    // accepted false: the runtime was not running, so nothing needed to
    // reload. The saved settings are used the next time it loads.
    const note = res && res.accepted === false ? res.message : null;
    st.impact = { ...st.impact, runtime_reload: false, changed: st.impact.changed, note };
    renderTop();
  } catch (e) {
    st.message = "Reload failed: " + e.message;
    renderTop();
  }
}

// restartService asks the service to restart itself, then waits for the
// API to answer again (with the new listener settings) and reloads.
async function restartService() {
  if (!window.confirm("Restart the Benchwarmer service now? The model is unloaded first, and inference is unavailable for a minute or so.")) return;
  let previous;
  try {
    previous = (await get("api/v1/health", { interactive: true, timeoutMs: 5000 })).instance_id;
    if (!previous) throw new Error("This service does not report a restart identity. Restart it through Windows Services.");
    await post("api/v1/service/restart");
  } catch (e) {
    st.message = "Restart failed: " + e.message;
    renderTop();
    return;
  }
  st.impact = null;
  st.message = "Restarting the service...";
  renderTop();
  const restarted = await waitForRestart(previous,
    timeoutMs => get("api/v1/health", { interactive: false, timeoutMs }));
  if (restarted) {
    location.reload();
    return;
  }
  st.message = "The service restart could not be confirmed after two minutes. If the management address changed, open the dashboard at the new address; otherwise see C:\\ProgramData\\Benchwarmer\\logs\\restart.log and the Windows event log.";
  renderTop();
}

function renderNav() {
  const item = (s) => {
    const n = [...st.errors.keys(), ...st.clientErrors.keys()].filter((p) => sectionFor(p) === s.key).length;
    return h("li", null, h("a", {
      href: "#/config/" + s.key,
      "aria-current": s.key === st.section ? "page" : null,
    }, s.label, n ? h("span", { class: "count", "aria-label": n + " problems" }, String(n)) : null));
  };
  replace(els.nav, h("ul", null, SECTIONS.filter(s => !s.advanced).map(item)),
    h("details", { open: SECTIONS.some(s => s.advanced && s.key === st.section) },
      h("summary", null, "Advanced"), h("ul", null, SECTIONS.filter(s => s.advanced).map(item))));
}

function switchSection(key) {
  if (!SECTIONS.some((s) => s.key === key)) key = "runtime";
  if (key === st.section) return;
  if (st.section === "json" && !applyJSON()) {
    history.replaceState(null, "", "#/config/json");
    return;
  }
  st.section = key;
  appView.query = "";
  appView.gpu = null;
  if (key !== "json") st.jsonText = null;
  renderNav();
  renderSection();
}

function jumpTo(path) {
  const sec = sectionFor(path);
  if (sec !== st.section) {
    if (st.section === "json" && !applyJSON()) return;
    st.section = sec;
    appView.query = "";
    appView.gpu = null;
    history.replaceState(null, "", "#/config/" + sec);
    renderNav();
    renderSection();
  }
  let p = path;
  for (;;) {
    const el = document.getElementById(idFor(p)) ||
      document.querySelector("[data-path=\"" + CSS.escape(p) + "\"] input, [data-path=\"" + CSS.escape(p) + "\"] select");
    if (el) {
      for (let parent = el.parentElement; parent; parent = parent.parentElement) if (parent.tagName === "DETAILS") parent.open = true;
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
    default: {
      const defs = sectionDefs(st.section);
      content = [defs.filter(g => !g.advanced).map(group),
        defs.some(g => g.advanced) && h("details", null, h("summary", null, "Advanced settings"), defs.filter(g => g.advanced).map(group))];
    }
  }
  replace(els.body, h("h2", null, sec.label), sec.intro && h("p", { class: "intro" }, sec.intro),
    st.section === "runtime" ? h("div", { id: "compatibility-guidance", class: "notice info", role: "status", "aria-live": "polite" }) : null, content);
  updateCompatibility();
  updateModelChoices();
}

function updateCompatibility() {
  if (!els || st.section !== "runtime") return;
  const box = document.getElementById("compatibility-guidance");
  if (!box || !st.draft) return;
  const model = (st.setup?.models || []).find(m => m.path === st.draft.runtime?.model_path);
  const runtime = (st.setup?.runtimes || []).find(r => r.path === st.draft.runtime?.executable);
  const text = compatibilityMessage(model, runtime);
  if (box.textContent !== text) box.textContent = text;
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
        profilePicker("default_profile", "Default profile", opts, "Applies whenever no schedule is active."),
        profilePicker("ai_priority_profile", "AI Priority profile", opts, "Overrides schedules while AI Priority is on. Safety limits, critical contention and confirmed gaming still win."))),
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

function profileSummary(profile) {
  if (!profile) return "Profile is unavailable.";
  const yes = (v) => v ? "yes" : "no";
  return [profile.description || "No description.",
    "Grace " + profile.grace + "; under pressure " + profile.grace_under_pressure + "; cooldown " + profile.cooldown + ".",
    "Drain on game process: " + yes(profile.drain_on_game_process) + "; ambiguous signals: " + yes(profile.drain_on_ambiguous) + "; soft contention: " + yes(profile.drain_on_soft_contention) + ".",
    "Soft GPU " + profile.external_util_soft_pct + "%, VRAM " + profile.external_vram_soft_mib + " MiB for " + profile.soft_window + "; game confirmation " + profile.game_confirm_util_pct + "% or " + profile.game_confirm_vram_mib + " MiB.",
  ].join(" ");
}

export function refreshProfilePreviews(draft, root) {
  for (const preview of root.querySelectorAll("[data-profile-preview]")) {
    const name = getPath(draft, preview.dataset.profilePreview);
    const text = profileSummary(draft.profiles?.[name]);
    if (preview.textContent !== text) preview.textContent = text;
  }
}

function profilePicker(path, label, opts, help) {
  const picker = field(ctx, { path, label, type: "select", options: opts, help });
  const preview = h("p", { class: "help", dataset: { profilePreview: path } }, profileSummary(st.draft.profiles?.[getPath(st.draft, path)]));
  return h("div", null, picker, preview);
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
          profilePicker(p + ".profile", "Profile", opts, "Used during this schedule in normal mode. Pause stops AI; AI Priority uses its own profile. Safety limits and critical contention always win."),
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

const appView = { query: "", gpu: null, at: 0 };

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
      type: "button", id, "aria-label": label + ": rule " + (i + 1), disabled: i + d < 0 || i + d >= list.length,
      onclick: () => structural(() => {
        const [x] = list.splice(i, 1);
        list.splice(i + d, 0, x);
      }, idFor("applications[" + (i + d) + "]") + (d < 0 ? "-up" : "-down")),
    }, label);
    const classField = field(ctx, { path: p + ".class", label: "Class", type: "select", options: CLASSES });
    const consequence = h("p", { class: "help" });
    const explain = () => { consequence.textContent = ({ game: "A confirmed game makes AI yield the GPU.", launcher: "A GPU-using child may be treated as a game.", ordinary: "GPU use contributes to ordinary contention.", ignore: "Ignored for application classification; GPU pressure may still affect policy." })[r.class] || "Choose a class."; };
    classField.querySelector("select").addEventListener("change", () => structural(() => {}, idFor(p + ".class")));
    explain();
    return h("li", { class: "rule", dataset: { path: p, ruleClass: r.class || "ordinary" } },
      h("div", { class: "rule-head" }, h("span", { class: "rule-num" }, "#" + (i + 1)),
        h("span", { class: "rule-origin " + (r.id ? "builtin" : "local"),
          title: r.id ? "Shipped with Benchwarmer. Edits stay; deleting it keeps it deleted." : "Added on this PC." },
          r.id ? "Built-in" : "Local"),
        h("span", { class: "rule-actions" },
          move(-1, "Earlier in rule order", idFor(p) + "-up"), move(1, "Later in rule order", idFor(p) + "-down"),
          h("button", { type: "button", class: "danger", "aria-label": "Remove rule " + (i + 1),
            onclick: () => structural(() => list.splice(i, 1), "add-rule") }, "Remove"))),
      errorSlot(ctx, p),
      h("div", { class: "fields" },
        field(ctx, { path: p + ".name", label: "Name", placeholder: "optional" }),
        h("div", { class: "field" }, h("label", { for: kindId }, "Matcher type"), kindSel),
        valueField,
        classField), consequence);
  });
  const search = h("input", { id: "app-search", type: "search", placeholder: "Search rules", value: appView.query });
  const groups = ["game", "launcher", "ignore", "ordinary"].map(cls => {
    const members = rows.filter((_, i) => (list[i].class || "ordinary") === cls);
    return h("section", { class: "app-group" }, h("h3", null, ({game:"Games", launcher:"Launchers", ignore:"Ignored", ordinary:"Ordinary"})[cls]), h("ol", { class: "rules" }, members));
  });
  const applyQuery = () => {
    const query = search.value.toLowerCase().trim();
    appView.query = search.value;
    rows.forEach((row, i) => { row.hidden = !searchableValues(list[i]).includes(query); });
    snapshot.querySelectorAll("[data-process-search]").forEach(row => { row.hidden = !row.dataset.processSearch.includes(query); });
  };
  search.addEventListener("input", applyQuery);
  const snapshot = h("div", null, h("p", null, "Loading GPU users..."));
  async function refreshSnapshot() {
    try {
      const status = await get("api/v1/status", { interactive: false });
      appView.gpu = status.gpu || {};
      appView.at = performance.now();
      showSnapshot(appView.gpu);
    } catch (e) { replace(snapshot, h("p", null, "Could not load GPU users: " + e.message), h("button", { type: "button", onclick: refreshSnapshot }, "Retry")); }
  }
  function showSnapshot(gpu) {
    {
      const users = gpu.observed_gpu_users || [];
      const elapsed = performance.now() - appView.at;
      const fresh = snapshotFresh(gpu, elapsed);
      const ageNotice = h("p", null, Number.isFinite(gpu.process_scan_age_ms) ? "Process list scanned " + Math.round((gpu.process_scan_age_ms + elapsed) / 1000) + "s ago" + (fresh ? ". Processes may have exited since this scan." : ". This snapshot is stale; refresh before choosing an action.") : "No process scan available; refresh before choosing an action.");
      replace(snapshot, ageNotice,
        !users.length && h("p", null, "No external GPU users observed in this snapshot."),
        users.map(proc => {
          const java = (proc.name || "").toLowerCase() === "javaw.exe";
          const action = (cls, label) => h("button", { type: "button", disabled: !fresh || !proposedRule(proc, cls), onclick: async () => {
            let latest;
            try { latest = await get("api/v1/status", { interactive: false }); } catch (e) { window.alert("Could not verify the process: " + e.message); return; }
            const current = (latest.gpu?.observed_gpu_users || []).find(x => x.pid === proc.pid && x.name === proc.name && x.path === proc.path);
            if (!snapshotFresh(latest.gpu) || !current) { window.alert("This process has vanished or the snapshot is stale. Refresh and choose again."); await refreshSnapshot(); return; }
            const rule = proposedRule(current, cls);
            const duplicates = list.filter(r => sameMatcher(r, rule)).length;
            const consequence = cls === "game" ? "Benchwarmer will yield when this game is confirmed." : "Benchwarmer will ignore this process for application classification; GPU pressure may still affect policy.";
            const target = rule.path || rule.exe;
            if (!window.confirm("Stage " + label.toLowerCase() + " for " + target + "? " + consequence + (java ? " Other Java programs also use javaw.exe; this rule matches only this exact path." : "") + " The new rule will be first and take precedence over matching rules." + (duplicates ? " " + duplicates + " existing rule(s) have the same matcher and will remain below it." : "") + " This only changes the draft; save with authorization to apply it.")) return;
            structural(() => list.unshift(rule), idFor("applications[0]." + (rule.path ? "path" : "exe")));
          } }, label);
          return h("div", { class: "rule", dataset: { processSearch: searchableValues(proc) } }, h("strong", null, proc.name || "Unknown process"), h("p", null, "Executable: " + (proc.path || proc.name || "unavailable") + " · PID " + proc.pid + " · class " + (proc.class || "unclassified") + (proc.rule ? " · rule " + proc.rule : "")),
            java && h("p", { class: "help" }, "Minecraft Java may use javaw.exe, but other Java programs do too. " + (proc.path ? "A rule here matches only this exact executable path; it does not identify Minecraft." : "The full executable path is unknown, so no rule can be staged.")),
            action("game", "Treat as game"), " ", action("ignore", "Ignore"));
        }), h("button", { type: "button", onclick: refreshSnapshot }, "Refresh GPU users"));
      applyQuery();
      if (fresh) setTimeout(() => {
        if (!ageNotice.isConnected) return;
        ageNotice.textContent = "This snapshot is stale; refresh before choosing an action.";
        for (const button of snapshot.querySelectorAll(".rule button")) button.disabled = true;
      }, Math.max(0, 10000 - gpu.process_scan_age_ms - elapsed));
    }
  }
  if (appView.gpu) showSnapshot(appView.gpu); else refreshSnapshot();
  return [
    h("div", { class: "field" }, h("label", { for: "app-search" }, "Search processes and application rules"), search),
    h("section", { class: "group" }, h("h3", null, "Running GPU users"), snapshot),
    h("div", { class: "notice info" }, h("span", null,
      "Built-in rules ship with Benchwarmer; rules added in a newer version appear here after an upgrade, and one you delete stays deleted. Local rules were added on this PC. Rule numbers show evaluation order across groups: the lowest number wins. Put specific rules (a launcher's own executable) above broad ones (a whole library folder). Matching ignores case and treats / and \\ alike.")),
    h("p", { class: "help" }, "Rules apply in number order across all groups, so moving a rule changes its number, not only its place inside a group."),
    groups,
    h("button", { type: "button", id: "add-rule", onclick: () => structural(() => list.push({ exe: "", class: "game" }),
      idFor("applications[" + list.length + "].exe")) }, "Add rule at the end"),
  ];
}

// Advanced JSON

function jsonSection() {
  const ta = h("textarea", { id: "config-json", class: "json", rows: 30, spellcheck: "false",
    "aria-describedby": "config-json-help config-json-err", value: st.jsonText ?? JSON.stringify(st.draft, null, 2), oninput: () => { st.jsonText = ta.value; syncSaveBar(); } });
  const err = h("p", { class: "field-error", id: "config-json-err", hidden: true });
  els.json = { ta, err };
  return [
    h("p", { class: "help", id: "config-json-help" },
      "The full configuration as the API sees it. Durations are Go duration strings (\"15s\", \"5m0s\"). Leave \"<redacted>\" values as they are to keep the stored secret. Changes here apply to the draft when you switch sections or save."),
    ta, err,
    h("div", { class: "row-actions" },
      h("button", { type: "button", onclick: () => { if (applyJSON()) err.textContent = ""; } }, "Apply to draft"),
      h("button", { type: "button", onclick: () => { ta.value = JSON.stringify(st.draft, null, 2); st.jsonText = null; err.hidden = true; syncSaveBar(); } }, "Reset from draft")),
  ];
}

function applyJSON() {
  if (st.section !== "json" || !els || !els.json) return true;
  const { ta, err } = els.json;
  try {
    const v = JSON.parse(ta.value);
    if (!v || typeof v !== "object" || Array.isArray(v)) throw new Error("the top level must be an object");
    st.draft = v;
    st.jsonText = null;
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
  if (!els.saveState) {
    els.saveState = h("span", { class: "savestate", role: "status" });
    els.discard = h("button", { type: "button", onclick: discard }, "Discard");
    els.save = h("button", { type: "button", class: "primary", onclick: save }, "Save configuration");
    replace(els.bar, els.saveState, els.discard, els.save);
  }
  els.saveState.textContent = d ? "Unsaved changes" : "No unsaved changes";
  els.discard.disabled = !d;
}

function discard() {
  if (!window.confirm("Discard all unsaved changes?")) return;
  st.draft = clone(st.cfg);
  st.jsonText = null;
  st.errors.clear();
  st.clientErrors.clear();
  st.message = null;
  renderAll();
}

async function save() {
  if (st.saving || !applyJSON()) return;
  st.message = null;
  if (st.clientErrors.size) {
    renderTop();
    renderNav();
    focusSummary();
    return;
  }
  const submitted = clone(st.draft);
  let replaced = false;
  st.saving = true;
  try {
    const res = await put("api/v1/config", submitted);
    const unchanged = JSON.stringify(st.draft) === JSON.stringify(submitted) && st.jsonText == null;
    st.cfg = res.config;
    if (unchanged && JSON.stringify(res.config) !== JSON.stringify(st.draft)) {
      st.draft = clone(res.config);
      replaced = true;
    }
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
  st.saving = false;
  if (els) {
    renderTop();
    renderNav();
    if (replaced) {
      const focusId = document.activeElement?.id;
      renderSection();
      if (focusId) document.getElementById(focusId)?.focus();
    } else {
      refreshErrors(ctx, els.body);
    }
    syncSaveBar();
  }
  if (st.errors.size) focusSummary();
}

function focusSummary() {
  const s = document.getElementById("error-summary");
  if (s) {
    s.focus();
    s.scrollIntoView({ block: "start" });
  }
}
