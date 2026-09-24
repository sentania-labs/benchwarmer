// Form widgets bound to a config draft by dotted path ("profiles.normal.grace",
// "schedules[0].days"). Paths match the API's details[].field names so
// validation errors land next to the right input.
import { h } from "./dom.js";
import { DURATION_UNITS, formatGoDuration, parseGoDuration, splitDuration, unitMs } from "./format.js";

export function tokens(path) {
  const out = [];
  for (const part of path.split(".")) {
    const m = part.match(/^([^[\]]*)((?:\[\d+\])*)$/);
    if (!m) {
      out.push(part);
      continue;
    }
    if (m[1]) out.push(m[1]);
    for (const idx of m[2].matchAll(/\[(\d+)\]/g)) out.push(Number(idx[1]));
  }
  return out;
}

export function getPath(obj, path) {
  let o = obj;
  for (const t of tokens(path)) {
    if (o == null) return undefined;
    o = o[t];
  }
  return o;
}

export function setPath(obj, path, value) {
  const ts = tokens(path);
  let o = obj;
  for (let i = 0; i < ts.length - 1; i++) {
    if (o[ts[i]] == null) o[ts[i]] = typeof ts[i + 1] === "number" ? [] : {};
    o = o[ts[i]];
  }
  o[ts[ts.length - 1]] = value;
}

export const idFor = (path) => "f-" + path.replace(/[^A-Za-z0-9]+/g, "-");

// A form context: the draft, error maps, and a change callback.
//   ctx.draft, ctx.errors (Map path -> message from the server),
//   ctx.clientErrors (Map path -> message from local parsing),
//   ctx.changed() called after every edit.

function errorsFor(ctx, path) {
  const msgs = [];
  for (const m of [ctx.clientErrors, ctx.errors]) {
    for (const [p, msg] of m) {
      if (p === path || p.startsWith(path + "[")) msgs.push(p === path ? msg : p.slice(path.length) + ": " + msg);
    }
  }
  return msgs;
}

export function errorSlot(ctx, path) {
  const msgs = errorsFor(ctx, path);
  const el = h("p", { class: "field-error", id: idFor(path) + "-err", hidden: !msgs.length }, msgs.join("; "));
  return el;
}

function setClientError(ctx, path, wrap, msg) {
  if (msg) ctx.clientErrors.set(path, msg);
  else ctx.clientErrors.delete(path);
  const slot = wrap.querySelector(".field-error");
  const msgs = errorsFor(ctx, path);
  slot.textContent = msgs.join("; ");
  slot.hidden = !msgs.length;
  wrap.classList.toggle("invalid", msgs.length > 0);
}

// field renders one input for def:
//   { path, label, type: text|int|float|bool|duration|select|lines|durlist|time,
//     unit, help, min, max, step, options: [[value,label]], placeholder, list }
export function field(ctx, def) {
  const path = def.path;
  const id = idFor(path);
  const value = getPath(ctx.draft, path);
  const helpId = def.help ? id + "-help" : null;
  const errId = id + "-err";
  const describedBy = [helpId, errId].filter(Boolean).join(" ");
  const wrap = h("div", { class: "field", dataset: { path } });
  const err = errorSlot(ctx, path);
  if (!err.hidden) wrap.classList.add("invalid");
  const set = (v) => {
    setPath(ctx.draft, path, v);
    ctx.changed();
  };
  let control;
  const common = { id, "aria-describedby": describedBy, name: path };

  switch (def.type) {
    case "bool": {
      control = h("input", { ...common, type: "checkbox", checked: !!value, onchange: (e) => set(e.target.checked) });
      wrap.classList.add("check");
      wrap.append(control, h("label", { for: id }, def.label));
      break;
    }
    case "int":
    case "float": {
      control = h("input", {
        ...common, type: "number", inputmode: def.type === "int" ? "numeric" : "decimal",
        step: def.step || (def.type === "int" ? 1 : "any"), min: def.min, max: def.max,
        value: value == null ? "" : String(value),
        oninput: (e) => {
          const raw = e.target.value.trim();
          const n = def.type === "int" ? Number.parseInt(raw, 10) : Number.parseFloat(raw);
          if (raw === "" || !Number.isFinite(n) || (def.type === "int" && !/^-?\d+$/.test(raw))) {
            setClientError(ctx, path, wrap, def.type === "int" ? "enter a whole number" : "enter a number");
            return;
          }
          setClientError(ctx, path, wrap, null);
          set(n);
        },
      });
      break;
    }
    case "duration": {
      const ms = parseGoDuration(value || "0s") ?? 0;
      const parts = splitDuration(ms);
      const num = h("input", {
        ...common, type: "number", inputmode: "decimal", step: "any", min: 0, value: String(parts.value),
        "aria-label": def.label + " amount",
      });
      const unit = h("select", { id: id + "-unit", "aria-label": def.label + " unit" },
        DURATION_UNITS.map((u) => h("option", { value: u.key, selected: u.key === parts.unit }, u.label)));
      const update = () => {
        const raw = num.value.trim();
        const n = Number.parseFloat(raw);
        if (raw === "" || !Number.isFinite(n) || n < 0) {
          setClientError(ctx, path, wrap, "enter a duration of zero or more");
          return;
        }
        setClientError(ctx, path, wrap, null);
        set(formatGoDuration(n * unitMs(unit.value)));
      };
      num.addEventListener("input", update);
      unit.addEventListener("change", update);
      control = h("div", { class: "duration" }, num, unit);
      break;
    }
    case "select": {
      const opts = def.options.slice();
      if (value != null && value !== "" && !opts.some(([v]) => v === value)) opts.push([value, value + " (unknown)"]);
      control = h("select", { ...common, onchange: (e) => set(e.target.value) },
        opts.map(([v, l]) => h("option", { value: v, selected: v === value }, l)));
      break;
    }
    case "lines": {
      control = h("textarea", {
        ...common, rows: Math.max(3, (value || []).length + 1), spellcheck: "false",
        value: (value || []).join("\n"),
        oninput: (e) => set(e.target.value.split("\n").map((s) => s.trim()).filter((s) => s !== "")),
      });
      break;
    }
    case "durlist": {
      control = h("input", {
        ...common, type: "text", spellcheck: "false", placeholder: def.placeholder,
        value: (value || []).map((d) => friendlyGo(d)).join(", "),
        oninput: (e) => {
          const items = e.target.value.split(",").map((s) => s.trim()).filter(Boolean);
          const out = [];
          for (const it of items) {
            const ms = parseGoDuration(it.replace(/\s+/g, "").replace(/min$/, "m"));
            if (ms == null || ms <= 0) {
              setClientError(ctx, path, wrap, "“" + it + "” is not a duration (use 30m, 1h, 2h30m)");
              return;
            }
            out.push(formatGoDuration(ms));
          }
          setClientError(ctx, path, wrap, null);
          set(out);
        },
      });
      break;
    }
    case "time": {
      control = h("input", { ...common, type: "time", value: value || "", oninput: (e) => set(e.target.value) });
      break;
    }
    default: {
      control = h("input", {
        ...common, type: "text", spellcheck: "false", value: value ?? "", placeholder: def.placeholder,
        list: def.list, autocomplete: "off",
        oninput: (e) => set(e.target.value),
      });
    }
  }

  if (def.type !== "bool") {
    const label = h("label", { for: id }, def.label, def.unit ? h("span", { class: "unit" }, " (" + def.unit + ")") : null);
    wrap.append(label, control);
  }
  if (def.help) wrap.append(h("p", { class: "help", id: helpId }, def.help));
  wrap.append(err);
  return wrap;
}

// friendlyGo shortens "30m0s" to "30m" and "1h0m0s" to "1h" for editing.
export function friendlyGo(s) {
  return String(s).replace(/(\d)m0s$/, "$1m").replace(/(\d)h0m$/, "$1h");
}

export function fields(ctx, defs) {
  return defs.map((d) => field(ctx, d));
}
