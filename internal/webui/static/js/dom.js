// Minimal DOM helpers. Text always goes in through textContent, never
// innerHTML, so API data cannot inject markup.

// h builds an element. attrs keys: "class", "on<event>" handlers, boolean
// properties (checked, disabled, hidden, selected), "dataset" object, and
// anything else as an attribute. Children may be strings, nodes, arrays,
// null or false.
export function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      if (v == null || v === false) continue;
      if (k.startsWith("on") && typeof v === "function") {
        el.addEventListener(k.slice(2), v);
      } else if (k === "dataset") {
        Object.assign(el.dataset, v);
      } else if (k === "value") {
        el.value = v;
      } else if (k === "checked" || k === "selected" || k === "disabled" || k === "hidden" || k === "open") {
        el[k] = !!v;
      } else {
        el.setAttribute(k, v === true ? "" : String(v));
      }
    }
  }
  append(el, children);
  return el;
}

function append(el, children) {
  for (const c of children) {
    if (c == null || c === false) continue;
    if (Array.isArray(c)) append(el, c);
    else el.append(c instanceof Node ? c : String(c));
  }
}

export function clear(el) {
  while (el.firstChild) el.removeChild(el.firstChild);
  return el;
}

export function replace(el, ...children) {
  clear(el);
  append(el, children);
  return el;
}

const SVG = "http://www.w3.org/2000/svg";

function svg(viewBox, ...shapes) {
  const s = document.createElementNS(SVG, "svg");
  s.setAttribute("viewBox", viewBox);
  s.setAttribute("aria-hidden", "true");
  s.setAttribute("class", "icon");
  for (const [tag, a] of shapes) {
    const e = document.createElementNS(SVG, tag);
    for (const [k, v] of Object.entries(a)) e.setAttribute(k, v);
    s.append(e);
  }
  return s;
}

// Condition icons differ in shape, not only color: a circle with a check, a
// triangle with a pause, and an octagon with a bar.
export function conditionIcon(cond) {
  if (cond === "Available") {
    return svg("0 0 24 24", ["circle", { cx: 12, cy: 12, r: 10, fill: "currentColor" }],
      ["path", { d: "M7 12.5l3.2 3.2L17 9", class: "ink-stroke" }]);
  }
  if (cond === "Yielding") {
    return svg("0 0 24 24", ["path", { d: "M12 2L23 21H1z", fill: "currentColor" }],
      ["rect", { x: 9, y: 10, width: 2, height: 7, class: "ink" }],
      ["rect", { x: 13, y: 10, width: 2, height: 7, class: "ink" }]);
  }
  return svg("0 0 24 24", ["path", { d: "M7.8 2h8.4L22 7.8v8.4L16.2 22H7.8L2 16.2V7.8z", fill: "currentColor" }],
    ["rect", { x: 6.5, y: 10.5, width: 11, height: 3, class: "ink" }]);
}

export function warnIcon() {
  return svg("0 0 24 24", ["path", { d: "M12 2L23 21H1z", fill: "currentColor" }],
    ["rect", { x: 11, y: 8, width: 2, height: 7, class: "knock" }],
    ["rect", { x: 11, y: 16.5, width: 2, height: 2, class: "knock" }]);
}
