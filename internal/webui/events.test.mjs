import { test } from 'node:test';
import assert from 'node:assert/strict';

// Minimal DOM stub: enough for dom.js and the two page modules under test.
class Node {
  constructor(tag) {
    this.tagName = tag;
    this.children = [];
    this.dataset = {};
    this.style = {};
    this.textContent = '';
    this.attributes = {};
    this.hidden = false;
    this.open = false;
  }
  focus() { Node.active = this; }
  get firstElementChild() { return this.children[0] || null; }
  setAttribute(k, v) { this.attributes[k] = v; }
  contains(n) {
    return n === this || this.children.some((c) => c && (c === n || (c instanceof Node && c.contains(n))));
  }
  append(...cs) {
    for (const c of cs) this.children.push(c);
  }
}
Node.active = null;
globalThis.Node = Node;
globalThis.document = {
  createElement: (t) => new Node(t),
  createElementNS: (_ns, t) => new Node(t),
  get activeElement() { return Node.active; },
};

const fmt = await import('./static/js/format.js');
const events = await import('./static/js/events.js');

test('a state transition is a state change, not a problem', () => {
  assert.equal(fmt.eventKind({ type: 'state_changed', severity: 'notice' }), 'change');
  assert.equal(fmt.eventKind({ type: 'mode_changed', severity: 'info' }), 'change');
  assert.equal(fmt.KIND_LABELS.change, 'State change');
});

test('real faults are labelled problems', () => {
  for (const t of ['load_failed', 'runtime_crashed', 'kill_failed', 'config_rejected', 'device_lost']) {
    assert.equal(fmt.eventKind({ type: t, severity: 'critical' }), 'problem', t);
  }
  assert.equal(fmt.KIND_LABELS.problem, 'Problem');
});

test('ordinary steps are labelled steps', () => {
  for (const t of ['service_started', 'load_started', 'load_completed', 'drain_completed', 'vram_released']) {
    assert.equal(fmt.eventKind({ type: t, severity: 'info' }), 'step', t);
  }
});

test('an unknown type falls back to its severity, then to step', () => {
  assert.equal(fmt.eventKind({ type: 'brand_new', severity: 'critical' }), 'problem');
  assert.equal(fmt.eventKind({ type: 'brand_new', severity: 'notice' }), 'change');
  assert.equal(fmt.eventKind({ type: 'brand_new', severity: 'info' }), 'step');
  assert.equal(fmt.eventKind({ type: 'brand_new' }), 'step');
  assert.equal(fmt.eventKind(undefined), 'step');
});

test('state names are intelligible and unknown values are not blank', () => {
  assert.equal(fmt.stateLabel('COOLDOWN'), 'Waiting');
  assert.equal(fmt.stateLabel('DISABLED'), 'Paused');
  assert.equal(fmt.stateLabel('ERROR'), 'Problem');
  assert.equal(fmt.stateLabel('SOMETHING_NEW'), 'SOMETHING NEW');
  assert.equal(fmt.stateLabel('something_new'), 'Something new');
  assert.equal(fmt.stateLabel(''), 'unknown');
});

test('a transition reads as two named states, with a dash for a missing side', () => {
  assert.equal(fmt.transitionText('STOPPED', 'COOLDOWN'), 'Idle \u2192 Waiting');
  assert.equal(fmt.transitionText('', 'READY'), '\u2014 \u2192 Ready');
  assert.equal(fmt.transitionText('BUSY', ''), 'Busy \u2192 \u2014');
});

test('every type is in exactly one filter group, with a label', () => {
  const seen = new Set();
  for (const g of fmt.TYPE_GROUPS) {
    assert.ok(g.label && g.types.length, 'group needs a label and types');
    for (const t of g.types) {
      assert.equal(seen.has(t), false, 'duplicate type ' + t);
      seen.add(t);
    }
  }
  // The groups feed the filter, so the option list is the same size.
  const opts = events.typeOptions();
  assert.equal(opts.length, 1 + fmt.TYPE_GROUPS.length);
  assert.equal(opts[0].children.length, 1, 'the "All types" option has one text child');
  assert.equal(opts[0].children[0], 'All types');
  assert.equal(opts[1].children.length, fmt.TYPE_GROUPS[0].types.length);
});

test('filter groups agree with the kind each type reports', () => {
  for (const g of fmt.TYPE_GROUPS) {
    for (const t of g.types) {
      assert.equal(fmt.eventKind({ type: t }), g.kind, t);
      const strip = (s) => s.replace(/s$/, '');
      assert.equal(strip(g.label), strip(fmt.KIND_LABELS[g.kind]), t);
    }
  }
});

test('a row shows the kind and a readable transition, and keeps raw diagnostics', () => {
  const e = {
    id: 7, time: '2026-10-28T00:22:31Z', type: 'state_changed', severity: 'notice',
    prev_state: 'STOPPED', state: 'COOLDOWN', condition: 'Unavailable', profile: 'normal',
    rule: 'eligibility.cooldown', message: 'a game was using the GPU',
    data: { cooldown_ms: 300000 }, evidence: [{ name: 'external_vram_mib', value: '9900', threshold: '1000' }],
  };
  const { li, details, summary } = events.row(e, new Date('2026-10-28T00:23:00Z'), null);
  assert.equal(li.attributes.class, 'event sev-notice');
  assert.equal(details.children[0], summary);
  const flat = JSON.stringify(summary);
  assert.match(flat, /State change/);
  assert.match(flat, /Idle \u2192 Waiting/);
  // Diagnostic data is retained in the collapsed body.
  const body = JSON.stringify(details.children[1]);
  assert.match(body, /Raw states: STOPPED \u2192 COOLDOWN/);
  assert.match(body, /Type: state_changed/);
  assert.match(body, /Event #7/);
  assert.match(body, /external_vram_mib/);
  assert.match(body, /9900/);
});

test('an expanded event stays expanded across a refresh', () => {
  const e = { id: 12, time: '2026-10-28T00:22:31Z', type: 'mode_changed', severity: 'info', message: 'x' };
  const open = new Set(['12']);
  assert.equal(events.row(e, new Date(), open).details.open, true);
  assert.equal(events.row(e, new Date(), new Set()).details.open, false);
});
