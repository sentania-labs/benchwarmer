import { test } from 'node:test';
import assert from 'node:assert/strict';

// api.js touches sessionStorage and the token dialog; stub the few globals.
const store = new Map();
globalThis.sessionStorage = {
  getItem: (k) => store.get(k) ?? null,
  setItem: (k, v) => store.set(k, v),
  removeItem: (k) => store.delete(k),
};
const asked = [];
const el = () => ({ value: "", textContent: "", addEventListener() {}, removeEventListener() {}, focus() {} });
const dlg = { ...el(), open: false, showModal() { this.open = true; }, close() { this.open = false; } };
const form = el();
const input = el();
const reasonEl = el();
globalThis.document = {
  getElementById: (id) => ({ "token-dialog": dlg, "token-form": form, "token-input": input, "token-cancel": el(), "token-reason": reasonEl })[id],
};
let calls = [];
let responder = () => ({ status: 200, body: {} });
globalThis.fetch = async (path, init) => {
  calls.push({ path, init });
  const { status, body } = responder(path, init);
  return { ok: status < 400, status, text: async () => JSON.stringify(body) };
};
// Make the token dialog cancel at once and record why it opened.
form.addEventListener = (ev, fn) => { if (ev === "submit") form.submit = fn; };
const cancel = el();
document.getElementById = ((orig) => (id) => (id === "token-cancel" ? cancel : orig(id)))(document.getElementById);
cancel.addEventListener = (ev, fn) => { if (ev === "click") queueMicrotask(() => { asked.push(reasonEl.textContent); fn({ preventDefault() {} }); }); };

const { put, hasToken, modeTokenReason } = await import('./static/js/api.js');

test('a mode change with no token sends no credential and no dialog on success', async () => {
  calls = []; asked.length = 0;
  responder = () => ({ status: 200, body: { mode: "pause" } });
  await put("api/v1/mode", { mode: "pause", duration: "30m" }, { reason: "x" });
  assert.equal(calls.length, 1);
  assert.equal(calls[0].init.headers.Authorization, undefined);
  assert.equal(calls[0].init.headers["Content-Type"], "application/json");
  assert.equal(calls[0].init.credentials, "omit");
  assert.equal(calls[0].path, "api/v1/mode");
  assert.equal(asked.length, 0);
  assert.equal(hasToken(), false);
});

test('a 401 on AI first asks for the token with the given reason, and cancel keeps it out of storage', async () => {
  calls = []; asked.length = 0;
  responder = () => ({ status: 401, body: { error: "token", code: "unauthorized" } });
  await assert.rejects(put("api/v1/mode", { mode: "ai_priority" }, { reason: "AI first needs the management token." }), /Cancelled/);
  assert.deepEqual(asked, ["AI first needs the management token."]);
  assert.equal(store.size, 0);
  for (const c of calls) assert.ok(!c.path.includes("token"), "token must not be in a URL");
});

test('a 401 on Pause asks with a neutral reason that does not promise tokenless use', async () => {
  calls = []; asked.length = 0;
  responder = () => ({ status: 401, body: { error: "token", code: "unauthorized" } });
  await assert.rejects(put("api/v1/mode", { mode: "pause", duration: "30m" }, { reason: modeTokenReason("pause") }), /Cancelled/);
  assert.equal(asked.length, 1);
  assert.match(asked[0], /unless you are at this PC/);
  assert.doesNotMatch(asked[0], /do not\./);
  assert.equal(modeTokenReason("auto"), modeTokenReason("pause"));
  assert.equal(modeTokenReason("ai_priority"), "AI first needs the management token.");
});
