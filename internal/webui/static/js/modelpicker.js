// The model picker on Configuration > Runtime: choose a discovered model,
// read its GGUF header through the service, and review proposed settings
// before anything touches the draft. Nothing here saves; the owner accepts,
// edits, or declines each proposal, and Save stays a separate, authorized step.
import { h, replace } from "./dom.js";
import { get, ApiError } from "./api.js";
import { errorSlot, getPath, setPath, idFor } from "./fields.js";
import { applyRows, inspectUrl, modelOptions, parseEdit, proposalRows, summary, vramCaution } from "./modelplan.js";

const PATH = "runtime.model_path";
export const PICKER_ID = idFor(PATH);
// The custom path field lives in the section's Advanced settings (configdefs.js).
const CUSTOM_ID = "custom-model-path";

// Kept across visits to the page, like the config draft.
const ms = { path: "", state: "idle", result: null, error: "", seq: 0, edits: new Map(), skip: new Set(), note: "" };

// modelGroup builds the picker. deps: { ctx, setup() -> SetupInfo|null,
// draftChanged(), rerender() }.
export function modelGroup(deps) {
  const { ctx } = deps;
  const select = h("select", { id: PICKER_ID, "aria-describedby": "model-picker-help", onchange: onPick });
  const box = h("div", { class: "model-result", role: "status", "aria-live": "polite", tabindex: "-1", id: "model-result" });
  const checkBtn = h("button", { type: "button", onclick: () => inspect(getPath(ctx.draft, PATH)) }, "Check this model");

  function currentPath() {
    return getPath(ctx.draft, PATH) || "";
  }

  function updateSelect() {
    const info = deps.setup();
    const cur = currentPath();
    const { options, selected } = modelOptions(info ? info.models : [], cur);
    const opts = [];
    if (!cur) opts.push(h("option", { value: "", selected: true }, info && !options.length ? "No models found" : "Choose a model..."));
    for (const o of options) opts.push(h("option", { value: o.value, selected: o.value === selected }, o.label));
    replace(select, opts);
    checkBtn.hidden = !cur;
  }

  function onPick() {
    const p = select.value;
    if (!p) return;
    setPath(ctx.draft, PATH, p);
    const adv = document.getElementById(CUSTOM_ID);
    if (adv) adv.value = p;
    ctx.changed();
    inspect(p);
  }

  // The custom path box commits on change (blur or Enter), not per key.
  function onCustom(e) {
    if (e.target.id !== CUSTOM_ID) return;
    updateSelect();
    const p = currentPath();
    if (p) inspect(p);
    else {
      ms.state = "idle";
      renderResult();
    }
  }

  async function inspect(path) {
    if (!path) return;
    const seq = ++ms.seq;
    Object.assign(ms, { path, state: "loading", result: null, error: "", note: "" });
    ms.edits.clear();
    ms.skip.clear();
    renderResult();
    try {
      const res = await get(inspectUrl(path, getPath(ctx.draft, "runtime.context_size")), { interactive: false });
      if (seq !== ms.seq) return;
      ms.result = res;
      ms.state = "done";
    } catch (e) {
      if (seq !== ms.seq) return;
      ms.error = e instanceof ApiError && e.status === 429 ? "The service is busy reading another model. Try again in a moment."
        : "Could not read the model file: " + e.message;
      ms.state = "error";
    }
    renderResult();
  }

  function renderResult() {
    const kids = [];
    if (ms.state === "loading") kids.push(h("p", null, "Reading the model file..."));
    else if (ms.state === "error") kids.push(h("p", { class: "field-error" }, ms.error));
    else if (ms.state === "done") kids.push(...resultBody());
    else if (ms.note) kids.push(h("p", null, ms.note));
    replace(box, kids);
  }

  function resultBody() {
    const r = ms.result;
    if (!r || r.path !== currentPath() || ms.path !== currentPath()) {
      return [h("p", { class: "help" }, "The model path changed since it was last checked. Use Check this model to read it again.")];
    }
    if (r.status === "not_available") {
      return [h("p", { class: "help" }, "This path has not been checked. After you save the configuration with it, use Check this model to read it.")];
    }
    if (r.status !== "ok") {
      return [h("div", { class: "notice warn" }, h("span", null, r.message || "This file could not be inspected."),
        " ", h("strong", null, "No settings were proposed or changed."))];
    }
    const out = [];
    out.push(h("dl", { class: "facts" }, summary(r).map(([k, v]) => [h("dt", null, k), h("dd", null, v)])));
    const c = r.compatibility || {};
    out.push(h("p", { class: "help" }, h("strong", null, c.status === "incompatible" ? "Known not to work: " : "Runtime compatibility not checked. "),
      c.message || ""));
    const rows = proposalRows(r, ctx.draft);
    if (!rows.length) {
      out.push(h("p", null, h("strong", null, "No settings were proposed.")),
        h("ul", { class: "notes" }, ((r.plan && r.plan.notes) || []).map((n) => h("li", null, n))));
    } else {
      out.push(h("h3", null, "Proposed settings"),
        h("p", { class: "help", id: "proposal-help" }, "Nothing has been changed yet. Untick a setting to leave it as it is, or edit the proposed value, then apply. You can still change everything afterwards, and nothing is saved until you press Save configuration."),
        h("div", { class: "proposals", role: "group", "aria-describedby": "proposal-help" }, rows.map(rowEl)),
        h("div", { class: "row-actions" },
          h("button", { type: "button", class: "primary", onclick: () => apply(rows) }, "Apply selected to draft"),
          h("button", { type: "button", onclick: decline }, "Keep my settings")));
    }
    if (rows.length && r.plan && r.plan.notes && r.plan.notes.length) {
      out.push(h("details", { class: "notes" }, h("summary", null, "What these estimates do not know"),
        h("ul", null, r.plan.notes.map((n) => h("li", null, n)))));
    }
    if (ms.note) out.push(h("p", { class: "model-note" }, ms.note));
    return out;
  }

  function rowEl(row) {
    const id = idFor("proposal." + row.path);
    const raw = ms.edits.has(row.path) ? ms.edits.get(row.path) : String(row.after);
    const err = h("p", { class: "field-error", id: id + "-err", hidden: true });
    const input = h("input", { id, type: "number", inputmode: "numeric", min: 0, step: 1, value: raw, "aria-label": "Proposed " + row.label.toLowerCase() + " (" + row.unit + ")", "aria-describedby": id + "-why " + id + "-err",
      oninput: (e) => {
        ms.edits.set(row.path, e.target.value);
        const p = parseEdit(row.path, e.target.value);
        err.textContent = p.error || "";
        err.hidden = !p.error;
      } });
    const check = h("input", { type: "checkbox", id: id + "-use", checked: !ms.skip.has(row.path),
      onchange: (e) => { if (e.target.checked) ms.skip.delete(row.path); else ms.skip.add(row.path); } });
    return h("div", { class: "proposal" },
      h("div", { class: "proposal-head" }, check, h("label", { for: id + "-use" }, row.label, h("span", { class: "unit" }, " (" + row.unit + ")"))),
      h("p", { class: "change" },
        h("span", { class: "was" }, "Now: ", h("strong", null, row.before == null ? "not set" : Number(row.before).toLocaleString("en-US"))),
        h("span", { class: "to" }, "Proposed ", input)),
      err,
      h("p", { class: "help", id: id + "-why" }, "Because " + row.reason + " (" + row.certainty + ")."));
  }

  function apply(rows) {
    const take = [];
    for (const row of rows) {
      if (ms.skip.has(row.path)) continue;
      const raw = ms.edits.has(row.path) ? ms.edits.get(row.path) : String(row.after);
      const p = parseEdit(row.path, raw);
      if (p.error) {
        const el = document.getElementById(idFor("proposal." + row.path));
        const slot = document.getElementById(idFor("proposal." + row.path) + "-err");
        if (slot) {
          slot.textContent = row.label + ": " + p.error;
          slot.hidden = false;
        }
        if (el) el.focus();
        return;
      }
      take.push({ ...row, after: p.value });
    }
    const n = applyRows(ctx.draft, take);
    ms.note = n ? "Applied " + n + (n === 1 ? " setting" : " settings") + " to the draft. Review it, then press Save configuration."
      : "No settings were applied.";
    const caution = vramCaution(rows, take);
    if (caution) ms.note += " " + caution;
    ms.state = "idle";
    ms.edits.clear();
    ms.skip.clear();
    ctx.changed();
    deps.rerender();
    focusResult();
  }

  function decline() {
    ms.note = "No settings were changed.";
    ms.state = "idle";
    ms.edits.clear();
    ms.skip.clear();
    renderResult();
    focusResult();
  }

  function focusResult() {
    // apply() re-renders the section, so the live box is a new element.
    const el = document.getElementById("model-result");
    if (el) el.focus();
  }

  const info = deps.setup();
  const dir = info && info.models_dir ? info.models_dir : "the models folder";
  const el = h("fieldset", { class: "group", id: "model-picker", dataset: { path: PATH } },
    h("legend", null, "Model"),
    h("div", { class: "field" }, h("label", { for: PICKER_ID }, "Model"), select,
      h("p", { class: "help", id: "model-picker-help" }, "Models found in " + dir + ". Copy a .gguf file there and reload this page to see it. Choosing one reads its header (it is never run) and proposes settings you can accept or decline. To use a model at another path, open Advanced settings.")),
    h("div", { class: "row-actions" }, checkBtn),
    errorSlot(ctx, PATH), box);
  updateSelect();
  if (ms.state === "done" && ms.path !== currentPath()) ms.state = "idle";
  renderResult();
  return { el, updateSelect, onCustom };
}

// resetModelPicker forgets any result, for when the draft is reloaded or
// discarded.
export function resetModelPicker() {
  ms.seq++;
  Object.assign(ms, { path: "", state: "idle", result: null, error: "", note: "" });
  ms.edits.clear();
  ms.skip.clear();
}

