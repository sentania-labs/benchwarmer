import { test } from 'node:test';
import assert from 'node:assert/strict';
import { field } from './static/js/fields.js';

class Element {
  constructor(tag) { this.tagName = tag; this.children = []; this.dataset = {}; this.classList = { add() {}, toggle() {} }; this.listeners = {}; }
  setAttribute(k, v) { this[k] = v; }
  append(...items) { this.children.push(...items); }
  replaceChildren(...items) { this.children = items; }
  addEventListener(k, fn) { this.listeners[k] = fn; }
  querySelector(selector) { return this.walk().find(e => selector === '.field-error' && e.class === 'field-error'); }
  querySelectorAll(selector) { return selector === '.argv-row input[type="text"]' ? this.walk().filter(e => e.tagName === 'input' && e.type === 'text') : []; }
  walk() { return this.children.flatMap(e => e instanceof Element ? [e, ...e.walk()] : []); }
  focus() {}
  fire(event) { this.listeners[event]?.({ target: this }); }
}
globalThis.Node = Element;
globalThis.document = { createElement: tag => new Element(tag) };

test('argument editor preserves arbitrary tokens and explicit empty values', () => {
  const original = ['--flag', '--repeat', 'a b', '--repeat', '', '-3', '<redacted>'];
  const ctx = { draft: { runtime: { args: [...original] } }, errors: new Map(), clientErrors: new Map(), changed() {} };
  const widget = field(ctx, { path: 'runtime.args', label: 'Arguments', type: 'argv' });
  assert.deepEqual(ctx.draft.runtime.args, original);
  const inputs = widget.walk().filter(e => e.tagName === 'input' && e.type === 'text');
  inputs[2].value = 'new value'; inputs[2].fire('input');
  assert.deepEqual(ctx.draft.runtime.args, ['--flag', '--repeat', 'new value', '--repeat', '', '-3', '<redacted>']);
  const checkbox = widget.walk().find(e => e.tagName === 'input' && e.type === 'checkbox');
  checkbox.checked = true; checkbox.fire('change');
  assert.deepEqual(ctx.draft.runtime.args, ['--flag', '', '--repeat', 'new value', '--repeat', '', '-3', '<redacted>']);
  checkbox.checked = false; checkbox.fire('change');
  assert.deepEqual(ctx.draft.runtime.args, ['--flag', '--repeat', 'new value', '--repeat', '', '-3', '<redacted>']);
});
