// Pure helpers for the model picker: no DOM, so they run under node --test.
// The numbers themselves come from the server (GET api/v1/setup/model); this
// module only turns them into before/after rows and applies the ones the
// owner accepts.
import { getPath, setPath } from "./fields.js";

// Bounds the server enforces on the proposed fields (internal/config).
export const FIELD_META = {
  "runtime.context_size": { label: "Context size", unit: "tokens", min: 256, max: 1 << 20 },
  "runtime.required_free_vram_mib": { label: "Required free VRAM before loading", unit: "MiB", min: 0, max: 1 << 20 },
};

// llama.cpp general.file_type names (a subset; others show their number).
const FILE_TYPES = {
  0: "F32", 1: "F16", 2: "Q4_0", 3: "Q4_1", 7: "Q8_0", 8: "Q5_0", 9: "Q5_1", 10: "Q2_K", 11: "Q3_K_S",
  12: "Q3_K_M", 13: "Q3_K_L", 14: "Q4_K_S", 15: "Q4_K_M", 16: "Q5_K_S", 17: "Q5_K_M", 18: "Q6_K", 32: "BF16",
};

export const quantName = (t) => FILE_TYPES[t] || (t ? "type " + t : "");

export function baseName(p) {
  return String(p || "").split(/[\\/]/).pop();
}

const samePath = (a, b) => String(a).replace(/\//g, "\\").toLowerCase() === String(b).replace(/\//g, "\\").toLowerCase();

export function fmtMiB(n) {
  return Number(n).toLocaleString("en-US") + " MiB";
}

export function fmtBytes(b) {
  return b >= 1 << 30 ? (b / (1 << 30)).toFixed(1) + " GiB" : fmtMiB(Math.round(b / 1048576));
}

// modelOptions lists the discovered models for the picker. A configured path
// that is not among them is kept as its own selected option, never dropped,
// so opening the page cannot silently change the model.
export function modelOptions(models, currentPath) {
  const opts = (models || []).map((m) => ({ value: m.path, label: m.name + " (" + fmtBytes(m.size_bytes) + ")" }));
  const found = opts.find((o) => samePath(o.value, currentPath));
  let selected = found ? found.value : "";
  let outside = false;
  if (currentPath && !found) {
    opts.unshift({ value: currentPath, label: baseName(currentPath) + " (not in the models folder)" });
    selected = currentPath;
    outside = true;
  }
  return { options: opts, selected, outside };
}

// proposalRows pairs each server recommendation with the draft's current
// value. Rows whose value already matches are dropped; unknown fields are
// ignored so a newer server cannot make the UI write somewhere unexpected.
export function proposalRows(result, draft) {
  if (!result || result.status !== "ok" || !result.plan) return [];
  const rows = [];
  for (const r of result.plan.recommendations || []) {
    const meta = FIELD_META[r.path];
    if (!meta || !Number.isInteger(r.value)) continue;
    const before = getPath(draft, r.path);
    if (before === r.value) continue;
    rows.push({ path: r.path, label: meta.label, unit: meta.unit, before: before ?? null, after: r.value,
      reason: r.reason, certainty: r.certainty === "from_file" ? "from the model file" : "estimate" });
  }
  return rows;
}

// parseEdit validates an owner-edited proposal value.
export function parseEdit(path, raw) {
  const meta = FIELD_META[path];
  const s = String(raw).trim();
  if (!meta || !/^\d+$/.test(s)) return { error: "enter a whole number" };
  const n = Number(s);
  if (n < meta.min || n > meta.max) return { error: "must be between " + meta.min.toLocaleString("en-US") + " and " + meta.max.toLocaleString("en-US") };
  return { value: n };
}

// applyRows writes the accepted rows into the draft and returns how many
// changed. Nothing else in the draft is touched.
export function applyRows(draft, rows) {
  let n = 0;
  for (const r of rows) {
    if (!FIELD_META[r.path] || getPath(draft, r.path) === r.after) continue;
    setPath(draft, r.path, r.after);
    n++;
  }
  return n;
}

// summary describes the model in a few plain facts.
export function summary(result) {
  const m = result && result.metadata;
  if (!m) return [];
  const out = [];
  if (m.name) out.push(["Name", m.name]);
  if (m.architecture) out.push(["Architecture", m.architecture]);
  if (m.size_label) out.push(["Size", m.size_label]);
  const q = quantName(m.file_type);
  if (q) out.push(["Quantization", q]);
  if (result.size_bytes) out.push(["File size", fmtBytes(result.size_bytes)]);
  if (m.block_count) out.push(["Layers", String(m.block_count)]);
  if (m.context_length) out.push(["Trained context", m.context_length.toLocaleString("en-US") + " tokens"]);
  if (m.expert_count) out.push(["Experts", m.expert_count + (m.expert_used_count ? " (" + m.expert_used_count + " used per token)" : "")]);
  return out;
}

export function inspectUrl(path, context) {
  let u = "api/v1/setup/model?path=" + encodeURIComponent(path);
  if (Number.isInteger(context) && context > 0) u += "&context=" + context;
  return u;
}
