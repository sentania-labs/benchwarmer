import { test } from 'node:test';
import assert from 'node:assert/strict';
import { ownerView, quickChoices, appliedText } from './static/js/owner.js';

const NOW = new Date('2026-09-24T19:46:00');
const iso = (mins) => new Date(NOW.getTime() + mins * 60000).toISOString();
const st = (owner, extra = {}) => ({ owner, mode: { mode: 'auto' }, gpu: {}, timers: {}, ...extra });
const view = (owner, extra) => ownerView(st(owner, extra), NOW);
const time = (mins) => new Date(NOW.getTime() + mins * 60000).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });

test('AI on is ok and says so once', () => {
  const v = view({ kind: 'available', severity: 'ok', cause: 'ready' });
  assert.equal(v.severity, 'ok');
  assert.match(v.headline, /AI is on/);
  assert.deepEqual(v.lines, []);
});

test('a game is a normal wait, not a fault, and promises no time', () => {
  const v = view({ kind: 'waiting', severity: 'wait', cause: 'game', app: 'game.exe', resume_delay_seconds: 300 });
  assert.equal(v.severity, 'wait');
  assert.match(v.headline, /game \(game\.exe\) is using the GPU/);
  assert.match(v.lines.join(' '), /about 5 min after the game stops/);
  assert.doesNotMatch(v.lines.join(' '), /around \d/);
});

test('stepping aside reads differently from already off', () => {
  assert.match(view({ kind: 'waiting', severity: 'wait', cause: 'game', yielding: true }).headline, /stepping aside/);
});

test('a cooldown after a game names the game, not the rule, and qualifies the time', () => {
  const v = view({ kind: 'waiting', severity: 'wait', cause: 'cooldown', after: 'game', return_at: iso(5) });
  assert.equal(v.severity, 'wait');
  assert.match(v.headline, /A game was using the GPU/);
  assert.doesNotMatch(v.headline + v.lines.join(' '), /contention|tier|eligibility|COOLDOWN/);
  assert.equal(v.lines.length, 1);
  assert.ok(v.lines[0].includes(time(5)));
  assert.match(v.lines[0], /if the GPU stays free/);
});

test('a cooldown with no known end promises no time', () => {
  const v = view({ kind: 'waiting', severity: 'wait', cause: 'cooldown', after: 'other' });
  assert.deepEqual(v.lines, []);
});

test('startup and wake waits are normal', () => {
  for (const cause of ['startup', 'resume', 'drained', 'recovering', 'suppressed']) {
    const v = view({ kind: 'waiting', severity: 'wait', cause, return_at: iso(2) });
    assert.equal(v.severity, 'wait', cause);
    assert.match(v.lines[0], /if the GPU is free/, cause);
  }
});

test('low GPU memory gives no time', () => {
  const v = view({ kind: 'waiting', severity: 'wait', cause: 'low_vram' });
  assert.doesNotMatch(v.headline + v.lines.join(' '), /around \d/);
});

test('a timed pause says when, and later if another timer holds the load', () => {
  const mode = { mode: 'pause', until: iso(60) };
  let v = view({ kind: 'paused', severity: 'paused', cause: 'paused', return_at: iso(60) }, { mode });
  assert.equal(v.severity, 'paused');
  assert.ok(v.headline.includes(time(60)));
  assert.match(v.lines[0], /Then AI starts again, if the GPU is free/);
  v = view({ kind: 'paused', severity: 'paused', cause: 'paused', return_at: iso(70) }, { mode });
  assert.ok(v.lines[0].includes(time(70)));
  assert.match(v.lines[0], /if the GPU is free by then/);
});

test('an open-ended pause promises nothing and says how to end it', () => {
  const v = view({ kind: 'paused', severity: 'paused', cause: 'paused' }, { mode: { mode: 'pause' } });
  assert.match(v.headline, /until you turn it back on/);
  assert.match(v.lines[0], /Back to normal/);
});

test('overheating is a problem with advice, and shows the temperature when known', () => {
  let v = view({ kind: 'problem', severity: 'problem', cause: 'too_hot' }, { gpu: { temperature_c: 96.4 } });
  assert.equal(v.severity, 'problem');
  assert.match(v.headline, /too hot \(96 °C\)/);
  assert.match(v.lines.join(' '), /fans/);
  v = view({ kind: 'problem', severity: 'problem', cause: 'too_hot' }, { gpu: { temperature_c: null } });
  assert.doesNotMatch(v.headline, /°C|NaN|null/);
});

test('setup, crash and GPU faults are problems with a way forward', () => {
  const setup = view({ kind: 'problem', severity: 'problem', cause: 'setup', detail: 'no model file found' });
  assert.equal(setup.severity, 'problem');
  assert.match(setup.headline, /no model file found/);
  assert.equal(setup.action.href, '#/config/runtime');
  const crash = view({ kind: 'problem', severity: 'problem', cause: 'crash', return_at: iso(16) });
  assert.equal(crash.severity, 'problem');
  assert.ok(crash.lines[0].includes(time(16)));
  assert.equal(crash.action.href, '#/events');
  assert.equal(view({ kind: 'problem', severity: 'problem', cause: 'gpu_fault' }).severity, 'problem');
  assert.equal(view({ kind: 'problem', severity: 'problem', cause: 'no_gpu_readings' }).severity, 'problem');
});

test('unknown stays unknown, never a guess', () => {
  for (const s of [undefined, {}, st(undefined), st({ kind: 'weird', severity: 'sparkly', cause: 'x' }), st({ kind: 'unknown', severity: 'unknown', cause: 'unknown' }), st({ kind: 'waiting', severity: 'wait', cause: 'never_heard_of_it' })]) {
    const v = ownerView(s, NOW);
    assert.equal(v.severity, 'unknown');
    assert.match(v.headline, /can't tell/);
  }
});

test('a fallback config raises attention without hiding the cause', () => {
  const v = view({ kind: 'available', severity: 'attention', cause: 'ready', config_fallback: true });
  assert.equal(v.severity, 'attention');
  assert.match(v.headline, /AI is on/);
  assert.match(v.lines.join(' '), /settings problem/);
});

test('a pause shows even when something else is the reason AI is off', () => {
  const v = view({ kind: 'problem', severity: 'problem', cause: 'too_hot' }, { mode: { mode: 'pause', until: iso(30) } });
  assert.match(v.lines.join(' '), /also paused until/);
  const first = view({ kind: 'available', severity: 'ok', cause: 'ready' }, { mode: { mode: 'ai_priority', until: iso(30) } });
  assert.match(first.lines.join(' '), /AI first is on until/);
});

test('the announcement key ignores clocks and temperatures but not real transitions', () => {
  const a = view({ kind: 'waiting', severity: 'wait', cause: 'cooldown', after: 'game', return_at: iso(5) });
  const b = view({ kind: 'waiting', severity: 'wait', cause: 'cooldown', after: 'game', return_at: iso(4) });
  assert.equal(a.key, b.key);
  const hot1 = view({ kind: 'problem', severity: 'problem', cause: 'too_hot' }, { gpu: { temperature_c: 96 } });
  const hot2 = view({ kind: 'problem', severity: 'problem', cause: 'too_hot' }, { gpu: { temperature_c: 97 } });
  assert.equal(hot1.key, hot2.key);
  assert.notEqual(a.key, view({ kind: 'waiting', severity: 'wait', cause: 'game' }).key);
  assert.notEqual(a.key, view({ kind: 'waiting', severity: 'wait', cause: 'cooldown', after: 'game', yielding: true }).key);
});

test('quick pause choices come from the configured durations and label tomorrow with its time', () => {
  const opts = { pause_durations: ['30m0s', '1h0m0s', '2h0m0s', '4h0m0s'], ai_priority_durations: ['30m0s', '1h0m0s'] };
  const tomorrow = new Date('2026-09-25T08:00:00').toISOString();
  const c = quickChoices('pause', opts, tomorrow, NOW);
  assert.deepEqual(c.map((x) => x.duration), ['30m0s', '1h0m0s', '2h0m0s', 'until_tomorrow']);
  assert.match(c[3].label, /^Until .*8:00/);
  assert.match(c[3].label, /Fri/); // not today, so the day is named
  assert.deepEqual(quickChoices('ai_priority', opts, tomorrow, NOW).map((x) => x.duration), ['30m0s', '1h0m0s']);
  assert.deepEqual(quickChoices('pause', undefined, undefined, NOW), []);
});

test('confirmation text repeats what was chosen', () => {
  const tomorrow = new Date('2026-09-25T08:00:00').toISOString();
  assert.equal(appliedText('auto'), 'Back to normal.');
  assert.match(appliedText('pause', 'until_tomorrow', tomorrow, NOW), /^AI paused until .*8:00/);
  assert.equal(appliedText('pause', '1h0m0s'), 'AI paused for 1 h.');
  assert.equal(appliedText('pause', ''), 'AI paused until you turn it back on.');
});
