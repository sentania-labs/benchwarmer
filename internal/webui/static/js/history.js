// Saved configuration versions. Requests run only on explicit navigation or
// selection, never in the background while someone is editing a draft.
import { h, replace } from "./dom.js";
import { get, post } from "./api.js";

export function describeImpact(impact) {
  const out = [];
  if (impact.runtime_reload) out.push("The runtime will reload.");
  if (impact.service_restart) out.push("The service needs a restart.");
  if (!out.length) out.push("The changes apply live.");
  return out.join(" ");
}

function changeList(changes) {
  return changes.length ? h("ul", null, changes.map(c => h("li", null, h("strong", null, c.path), ": ", JSON.stringify(c.before), " → ", JSON.stringify(c.after)))) : h("p", null, "No settings differ.");
}

export function mountHistory(root, { isDirty, restored }) {
  let active = true;
  const status = h("p", { role: "status", tabindex: "-1" }, "Loading saved versions...");
  const list = h("div");
  const preview = h("div");
  replace(root, h("p", null, "Up to 20 saved versions are kept on this PC. Restoring creates a new saved version."), h("button", { type: "button", onclick: () => load() }, "Refresh history"), status, list, preview);
  const fail = message => { status.textContent = message; status.focus(); };
  async function load(announcement = "") {
    try {
      const result = await get("api/v1/config/history", { interactive: false });
      if (!active) return;
      const versions = result.versions || [];
      status.textContent = announcement || (versions.length ? "" : "No saved versions yet.");
      replace(list, h("ol", null, versions.map(v => h("li", null,
        h("span", null, new Date(v.time).toLocaleString() + " · " + v.actor + (v.restore_of ? " · restored" : "")), " ",
        h("button", { type: "button", onclick: () => showPreview(v.id) }, "Preview restore"),
        h("details", null, h("summary", null, "Changes in this save"), changeList(v.changes || []))))));
    } catch (e) { if (active) fail("Could not load history: " + e.message); }
  }
  async function showPreview(id) {
    if (isDirty()) { fail("Save or discard your draft before restoring."); return; }
    status.textContent = "Loading restore preview...";
    try {
      const p = await get("api/v1/config/restore-preview?id=" + encodeURIComponent(id), { interactive: false });
      if (!active) return;
      status.textContent = "";
      const changes = p.changes || [];
      const heading = h("h3", { tabindex: "-1" }, "Restore preview");
      replace(preview, heading, h("p", null, describeImpact(p.impact)),
        changeList(changes),
        h("button", { type: "button", onclick: async () => {
          if (isDirty()) { fail("Save or discard your draft before restoring."); return; }
          try {
            const result = await post("api/v1/config/restore", { id: p.id, revision: p.revision });
            restored(result);
            if (!active) return;
            replace(preview); await load("Configuration restored.");
          } catch (e) { if (active) fail("Restore failed: " + e.message); }
        } }, "Restore this version"));
      heading.focus();
    } catch (e) { if (active) fail("Could not preview restore: " + e.message); }
  }
  load();
  return () => { active = false; };
}
