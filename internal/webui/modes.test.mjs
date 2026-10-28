import { test } from 'node:test';
import assert from 'node:assert/strict';
import { adoptsOptions, defaultDuration, durationChoices, modeKey, followsServer, normalizeOptions } from './static/js/modes.js';
import { configNotice } from './static/js/format.js';

const opts = normalizeOptions({ pause_durations: ['15m0s', '1h30m0s'], ai_priority_durations: ['45m0s'] });

test('pause choices come from modes.pause_durations', () => {
  assert.deepEqual(durationChoices('pause', opts), ['15m0s', '1h30m0s', 'until_tomorrow', 'until_reboot', '']);
});
test('ai priority choices come from modes.ai_priority_durations and cannot run forever', () => {
  assert.deepEqual(durationChoices('ai_priority', opts), ['45m0s', 'until_tomorrow', 'until_reboot']);
});
test('missing options leave only the fixed choices', () => {
  assert.deepEqual(durationChoices('ai_priority', normalizeOptions(undefined)), ['until_tomorrow', 'until_reboot']);
});
test('a new form starts on a timed choice, not on until reboot', () => {
  const stock = normalizeOptions({ pause_durations: ['30m0s', '1h0m0s', '2h0m0s'], ai_priority_durations: ['30m0s', '1h0m0s'] });
  assert.equal(defaultDuration('pause', stock), '1h0m0s');
  assert.equal(defaultDuration('ai_priority', stock), '1h0m0s');
  assert.equal(defaultDuration('ai_priority', opts), '45m0s');
  assert.equal(defaultDuration('pause', normalizeOptions(undefined)), 'until_tomorrow');
});
test('the form follows a mode changed elsewhere', () => {
  const was = modeKey({ mode: 'auto' });
  assert.equal(followsServer(false, was, modeKey({ mode: 'pause', until: '2026-01-01T20:00:00Z' })), true);
  assert.equal(followsServer(false, was, modeKey({ mode: 'auto' })), false);
  assert.equal(followsServer(false, null, was), true); // first poll
});
test('a pause that expired puts the form back on auto', () => {
  const paused = modeKey({ mode: 'pause', until: '2026-01-01T20:00:00Z' });
  assert.equal(followsServer(false, paused, modeKey({ mode: 'auto' })), true);
});
test('an edit in progress is never overwritten', () => {
  assert.equal(followsServer(true, modeKey({ mode: 'auto' }), modeKey({ mode: 'pause' })), false);
});

test('a rejected config.json names the field and is not called missing', () => {
  const msg = configNotice({ config_source: 'default', config_problem: 'listen.management: port 18491 is already used by runtime.port' });
  assert.match(msg, /listen\.management: port 18491 is already used by runtime\.port/);
  assert.match(msg, /built-in defaults/);
  assert.doesNotMatch(msg, /no usable config|no config\.json/i);
  assert.match(configNotice({ config_source: 'last_good', config_problem: 'x: y' }), /last known good/);
});
test('a first start with no file says so', () => {
  assert.match(configNotice({ config_source: 'default' }), /no config\.json existed/);
});

test('configured durations are adopted when first reported, whatever the form holds', () => {
  assert.equal(adoptsOptions(false, true, normalizeOptions(), opts), true);
});
test('changed durations wait while the mode form is being edited, then apply once clean', () => {
  const next = normalizeOptions({ pause_durations: ['5m0s'] });
  assert.equal(adoptsOptions(true, true, opts, next), false);
  assert.equal(adoptsOptions(true, false, opts, next), true);
});
test('unchanged durations are not re-applied', () => {
  assert.equal(adoptsOptions(true, false, opts, normalizeOptions({ pause_durations: ['15m0s', '1h30m0s'], ai_priority_durations: ['45m0s'] })), false);
});
