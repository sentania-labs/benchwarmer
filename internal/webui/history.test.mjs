import { test } from 'node:test';
import assert from 'node:assert/strict';
class Node {
  constructor(tag) { this.tagName = tag; this.children = []; this.dataset = {}; this.attributes = {}; }
  get firstChild() { return this.children[0] || null; }
  removeChild(c) { this.children.splice(this.children.indexOf(c), 1); }
  addEventListener(t, fn) { (this.listeners ||= {})[t] = fn; }
  setAttribute(k, v) { this.attributes[k] = v; }
  append(...items) { this.children.push(...items); }
  focus() { Node.active = this; }
}
globalThis.Node = Node;
globalThis.document = { createElement: tag => new Node(tag) };
const { mountHistory } = await import('./static/js/history.js');
const flush = () => new Promise(resolve => setTimeout(resolve, 0));
function find(node, tag, text) {
  if (!(node instanceof Node)) return null;
  if (node.tagName === tag && node.children.includes(text)) return node;
  for (const child of node.children) { const found = find(child, tag, text); if (found) return found; }
  return null;
}
test('preview and restore require explicit clicks and preserve revision', async () => {
  const calls = [];
  globalThis.fetch = async (url, init) => {
    calls.push([url, init]);
    const data = url.includes('restore-preview') ? { id: 'v1', revision: 'r1', changes: [{ path: 'runtime.args', before: '<redacted>', after: '<redacted>' }], impact: { runtime_reload: true } } :
      url.endsWith('/restore') ? { config: {} } : { versions: [{ id: 'v1', time: '2026-01-01T00:00:00Z', actor: 'api' }] };
    return { ok: true, text: async () => JSON.stringify(data) };
  };
  const root = new Node('main'); let dirty = false; let restored = 0;
  mountHistory(root, { isDirty: () => dirty, restored: () => restored++ });
  await flush();
  const button = find(root, 'button', 'Preview restore'); assert.ok(button);
  dirty = true; await button.listeners.click(); assert.equal(calls.length, 1);
  dirty = false; await button.listeners.click();
  assert.match(JSON.stringify(root), /runtime.args/); assert.equal(calls.length, 2); assert.equal(restored, 0);
  const restore = find(root, 'button', 'Restore this version'); assert.ok(restore);
  await restore.listeners.click();
  assert.equal(JSON.parse(calls[2][1].body).revision, 'r1'); assert.equal(restored, 1);
});
