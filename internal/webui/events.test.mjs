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
  get firstChild() { return this.children[0] || null; }
  removeChild(c) { this.children.splice(this.children.indexOf(c), 1); }
  addEventListener(t, fn) { (this.listeners ||= {})[t] = fn; }
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

test('a critical or warning event is a problem whatever its type', () => {
  assert.equal(fmt.eventKind({ type: 'config_recovered', severity: 'critical' }), 'problem');
  assert.equal(fmt.eventKind({ type: 'state_changed', severity: 'critical' }), 'problem');
  assert.equal(fmt.eventKind({ type: 'telemetry_degraded', severity: 'warning' }), 'problem');
  assert.equal(fmt.eventKind({ type: 'power_suspend_stop', severity: 'warning' }), 'problem');
  assert.equal(fmt.eventKind({ type: 'power_suspend_stop', severity: 'info' }), 'step');
  assert.equal(fmt.eventKind({ type: 'config_recovered', severity: 'notice' }), 'change');
});

test('a warning on a transition is a state change, not a problem', () => {
  for (const t of ['state_changed', 'decision_changed', 'mode_changed', 'mode_expired', 'cooldown_started', 'cooldown_reset', 'suppression_started', 'drain_started', 'drain_completed', 'grace_shortened', 'preempted']) {
    assert.notEqual(fmt.eventKind({ type: t, severity: 'warning', state: 'YIELDING' }), 'problem', t);
    assert.equal(fmt.eventKind({ type: t, severity: 'warning' }), fmt.eventKind({ type: t }), t);
  }
  assert.equal(fmt.eventKind({ type: 'decision_changed', severity: 'warning', state: 'COOLDOWN' }), 'change');
  assert.equal(fmt.eventKind({ type: 'drain_started', severity: 'warning' }), 'step');
  assert.equal(fmt.eventKind({ type: 'state_changed', severity: 'critical', state: 'ERROR' }), 'problem');
  assert.equal(fmt.eventKind({ type: 'state_changed', severity: 'notice', state: 'ERROR' }), 'problem');
  assert.equal(fmt.eventKind({ type: 'decision_changed', severity: 'critical', state: 'STOPPED' }), 'problem');
  for (const t of ['mode_changed', 'mode_expired', 'cooldown_reset', 'suppression_started', 'grace_shortened', 'preempted']) {
    assert.equal(fmt.eventKind({ type: t, severity: 'notice', state: 'ERROR' }), fmt.eventKind({ type: t }), t);
  }
  assert.equal(fmt.eventKind({ type: 'mode_changed', severity: 'notice', state: 'ERROR' }), 'change');
  assert.equal(fmt.eventKind({ type: 'decision_changed', severity: 'notice', state: 'ERROR' }), 'change');
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

test('an event stamped with only a current state is context, not a transition', () => {
  const e = { id: 21, time: '2026-10-28T00:22:31Z', type: 'mode_changed', severity: 'info', state: 'COOLDOWN', message: 'paused' };
  assert.equal(fmt.eventStateText(e), 'Service state: Waiting');
  assert.equal(fmt.eventRawStateText(e), 'Raw state: COOLDOWN');
  const { details, summary } = events.row(e, new Date('2026-10-28T00:23:00Z'), null);
  const head = JSON.stringify(summary);
  assert.match(head, /Service state: Waiting/);
  assert.doesNotMatch(head, /\u2192/);
  const body = JSON.stringify(details.children[1]);
  assert.match(body, /Raw state: COOLDOWN/);
  assert.doesNotMatch(body, /Raw states:/);
  assert.equal(fmt.eventStateText({ type: 'load_started' }), '');
  assert.equal(fmt.eventStateText({ prev_state: 'STOPPED', state: 'COOLDOWN' }), 'Idle \u2192 Waiting');
});

test('an expanded event stays expanded across a refresh', () => {
  const e = { id: 12, time: '2026-10-28T00:22:31Z', type: 'mode_changed', severity: 'info', message: 'x' };
  const open = new Set(['12']);
  assert.equal(events.row(e, new Date(), open).details.open, true);
  assert.equal(events.row(e, new Date(), new Set()).details.open, false);
});

function ev(id, extra) {
  return { id, time: '2026-10-28T00:22:31Z', type: 'mode_changed', severity: 'info', message: 'm' + id, ...extra };
}

function stubFetch(handler) {
  const calls = [];
  globalThis.fetch = async (path) => {
    calls.push(path);
    const r = await handler(path);
    return { ok: r.ok !== false, status: r.status || 200, text: async () => JSON.stringify(r.body || {}) };
  };
  return calls;
}

function mountEvents(page) {
  const main = new Node('main');
  const mounted = events.mount(main);
  return { main, mounted, filter: page };
}

const settle = () => new Promise((r) => setTimeout(r, 0));

function findAll(n, tag, out = []) {
  if (!(n instanceof Node)) return out;
  if (n.tagName === tag) out.push(n);
  for (const c of n.children) findAll(c, tag, out);
  return out;
}

test('a failed refresh keeps the list, expansions and focus', async () => {
  let fail = false;
  stubFetch(() => (fail
    ? { ok: false, status: 503, body: { error: 'down' } }
    : { body: { events: [ev(2), ev(1)] } }));
  const { main, mounted } = mountEvents();
  await settle();
  const list = findAll(main, 'ol')[0];
  const [first, second] = findAll(list, 'li');
  assert.equal(list.children.length, 2);
  const detailsBefore = findAll(second, 'details')[0];
  detailsBefore.open = true;
  findAll(first, 'summary')[0].focus();
  const focused = Node.active;
  fail = true;
  mounted.visibility(true);
  await settle();
  assert.equal(list.children.length, 2, 'the list survives a failed fetch');
  assert.equal(list.children[1], second, 'rows are not rebuilt on failure');
  assert.equal(Node.active, focused);
  assert.match(findAll(main, 'p')[0].textContent, /Could not load events/);
  mounted.unmount();
});

test('a refresh with new events keeps expansions and focus on the rebuilt rows', async () => {
  let events = [ev(3), ev(2), ev(1)];
  stubFetch(() => ({ body: { events } }));
  const { main, mounted } = mountEvents();
  await settle();
  const list = findAll(main, 'ol')[0];
  findAll(list.children[1], 'details')[0].open = true;
  findAll(list.children[2], 'summary')[0].focus();
  const focused = Node.active;
  events = [ev(4), ev(3), ev(2), ev(1)];
  mounted.visibility(true);
  await settle();
  assert.equal(list.children.length, 4);
  assert.equal(findAll(list.children[2], 'details')[0].open, true);
  assert.equal(findAll(list.children[1], 'details')[0].open, false);
  assert.notEqual(Node.active, focused, 'the summary is a new element');
  assert.equal(Node.active, findAll(list.children[3], 'summary')[0]);
  mounted.unmount();
});

test('changing the filter starts over instead of inheriting the old list', async () => {
  const many = Array.from({ length: 60 }, (_, i) => ev(100 - i));
  const calls = stubFetch((path) => (path.includes('type=load_failed')
    ? { body: { events: [ev(9, { type: 'load_failed', severity: 'critical' })] } }
    : { body: { events: many, next_before_id: 41 } }));
  const { main, mounted } = mountEvents();
  await settle();
  const select = findAll(main, 'select')[0];
  const list = findAll(main, 'ol')[0];
  findAll(list.children[0], 'details')[0].open = true;
  findAll(list.children[0], 'summary')[0].focus();
  select.value = 'load_failed';
  mounted.visibility(true);
  await settle();
  const last = calls[calls.length - 1];
  assert.match(last, /limit=50(&|$)/, 'the page size is not inherited from the old filter');
  assert.match(last, /type=load_failed/);
  assert.equal(list.children.length, 1);
  assert.equal(findAll(list.children[0], 'details')[0].open, false);
  mounted.unmount();
});

test('an empty list does not re-announce Loading on every refresh', async () => {
  stubFetch(() => ({ body: { events: [] } }));
  const { main, mounted } = mountEvents();
  await settle();
  const status = findAll(main, 'p')[0];
  assert.equal(status.textContent, 'No events.');
  let writes = 0;
  let text = status.textContent;
  Object.defineProperty(status, 'textContent', {
    get: () => text,
    set: (v) => { writes++; text = v; assert.notEqual(v, 'Loading...'); },
  });
  mounted.visibility(true);
  await settle();
  assert.equal(text, 'No events.');
  assert.ok(writes >= 1);
  mounted.unmount();
});

test('a filter changed while a fetch is in flight is reloaded fresh on the next refresh', async () => {
  let release;
  const gate = new Promise((r) => { release = r; });
  const calls = stubFetch(async (path) => {
    if (path.includes('type=load_failed')) return { body: { events: [ev(9, { type: 'load_failed', severity: 'critical' })] } };
    await gate;
    return { body: { events: [ev(2), ev(1)] } };
  });
  const { main, mounted } = mountEvents();
  const select = findAll(main, 'select')[0];
  select.value = 'load_failed';
  release();
  await settle();
  mounted.visibility(true);
  await settle();
  const list = findAll(main, 'ol')[0];
  assert.match(calls[calls.length - 1], /type=load_failed/);
  assert.equal(list.children.length, 1);
  mounted.unmount();
});

test('a repeating failure on background refresh leaves the status untouched', async () => {
  stubFetch(() => ({ ok: false, status: 401, body: { error: 'unauthorized' } }));
  const { main, mounted } = mountEvents();
  await settle();
  const status = findAll(main, 'p')[0];
  const button = findAll(status, 'button')[0];
  assert.ok(button, 'the sign-in prompt is shown');
  const written = [];
  Object.defineProperty(status, 'textContent', { get: () => '', set: (v) => written.push(v) });
  mounted.visibility(true);
  await settle();
  assert.equal(findAll(status, 'button')[0], button, 'the prompt is not rebuilt on a background refresh');
  assert.deepEqual(written, []);
  mounted.unmount();
});

test('a repeating generic failure does not rewrite the status on every tick', async () => {
  stubFetch(() => ({ ok: false, status: 503, body: { error: 'down' } }));
  const { main, mounted } = mountEvents();
  await settle();
  const status = findAll(main, 'p')[0];
  assert.match(status.textContent, /Could not load events: down/);
  let writes = 0;
  let text = status.textContent;
  Object.defineProperty(status, 'textContent', { get: () => text, set: (v) => { writes++; text = v; } });
  mounted.visibility(true);
  await settle();
  assert.equal(writes, 0);
  mounted.unmount();
});

test('an unchanged list is left untouched by a background refresh', async () => {
  stubFetch(() => ({ body: { events: [ev(2), ev(1)] } }));
  const { main, mounted } = mountEvents();
  await settle();
  const list = findAll(main, 'ol')[0];
  const [a, b] = list.children;
  mounted.visibility(true);
  await settle();
  assert.equal(list.children[0], a);
  assert.equal(list.children[1], b);
  mounted.unmount();
});

test('a new event on a background refresh rebuilds the list', async () => {
  let events = [ev(2), ev(1)];
  stubFetch(() => ({ body: { events } }));
  const { main, mounted } = mountEvents();
  await settle();
  const list = findAll(main, 'ol')[0];
  events = [ev(3), ev(2), ev(1)];
  mounted.visibility(true);
  await settle();
  assert.equal(list.children.length, 3);
  mounted.unmount();
});

test('a deeper list is refilled in several capped pages', async () => {
  const CAP = 5; // the API caps limit at 1000; a small cap exercises paging
  const all = Array.from({ length: 12 }, (_, i) => ev(100 - i));
  const calls = [];
  globalThis.fetch = async (path) => {
    calls.push(path);
    const limit = Math.min(CAP, Number(new URL("http://x/" + path).searchParams.get("limit") || 50));
    const before = Number(new URL("http://x/" + path).searchParams.get("before_id") || 0);
    const pool = before ? all.filter((e) => e.id < before) : all;
    const page = pool.slice(0, limit);
    return {
      ok: true, status: 200,
      text: async () => JSON.stringify({
        events: page,
        ...(pool.length > page.length ? { next_before_id: page[page.length - 1].id } : {}),
      }),
    };
  };
  const { main, mounted } = mountEvents();
  await settle();
  const list = findAll(main, 'ol')[0];
  assert.equal(list.children.length, 12, 'one page per response, all rows kept');
  assert.equal(calls.length, 3, 'the depth took three capped pages');
  assert.ok(calls.some((c) => c.includes('before_id=')), 'continuation pages used before_id');
  // A background refresh keeps the rendered depth even though each response
  // holds fewer events than that.
  const ids = findAll(list, 'details').map((d) => d.dataset.id);
  findAll(list.children[0], 'details')[0].open = true;
  findAll(list.children[1], 'summary')[0].focus();
  calls.length = 0;
  mounted.visibility(true);
  await settle();
  assert.equal(list.children.length, 12, 'the refresh kept the depth it rendered');
  assert.deepEqual(findAll(list, 'details').map((d) => d.dataset.id), ids);
  assert.equal(findAll(list.children[0], 'details')[0].open, true, 'the expansion survived');
  assert.equal(Node.active, findAll(list.children[1], 'summary')[0], 'focus stayed on the same row');
  mounted.unmount();
});

test('Load older events clicked during a background fetch is not lost', async () => {
  let release;
  const gate = new Promise((r) => { release = r; });
  let slow = false;
  const firstPage = Array.from({ length: 50 }, (_, i) => ev(200 - i));
  const calls = stubFetch(async (path) => {
    if (path.includes('before_id=50')) return { body: { events: [ev(1)] } };
    if (slow) await gate;
    return { body: { events: firstPage, next_before_id: 50 } };
  });
  const { main, mounted } = mountEvents();
  await settle();
  const list = findAll(main, 'ol')[0];
  assert.equal(list.children.length, 50, 'the first fill rendered one page');
  const more = findAll(main, 'button').find((b) => b.children[0] === 'Load older events');
  slow = true;
  mounted.visibility(true);
  await settle();
  more.listeners.click();
  release();
  await settle();
  await settle();
  assert.ok(calls.some((c) => c.includes('before_id=50')), 'the older page was requested');
  assert.equal(list.children.length, 51);
  mounted.unmount();
});

test('Load older events keeps the filter the visible rows were fetched with', async () => {
  const calls = stubFetch((path) => (path.includes('before_id=')
    ? { body: { events: [ev(1, { type: 'load_failed', severity: 'critical' })] } }
    : path.includes('type=load_failed')
      ? { body: { events: [ev(2, { type: 'load_failed', severity: 'critical' })], next_before_id: 2 } }
      : { ok: false, status: 503, body: { error: 'down' } }));
  const { main, mounted } = mountEvents();
  await settle();
  const select = findAll(main, 'select')[0];
  select.value = 'load_failed';
  mounted.visibility(true);
  await settle();
  const more = findAll(main, 'button').find((b) => b.children[0] === 'Load older events');
  select.value = 'state_changed';
  more.listeners.click();
  await settle();
  const older = calls.find((c) => c.includes('before_id='));
  assert.ok(older, 'the older page was requested');
  assert.match(older, /type=load_failed/);
  assert.doesNotMatch(older, /type=state_changed/);
  mounted.unmount();
});
