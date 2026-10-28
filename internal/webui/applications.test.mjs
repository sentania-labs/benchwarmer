import { test } from 'node:test';
import assert from 'node:assert/strict';
import { proposedRule, sameMatcher, snapshotFresh } from './static/js/applications.js';

test('staging uses exact paths and refuses a generic Java rule', () => {
  assert.deepEqual(proposedRule({name:'game.exe', path:'C:\\Games\\game.exe'}, 'game'), {path:'C:\\Games\\game.exe', class:'game'});
  assert.deepEqual(proposedRule({name:'game.exe'}, 'ignore'), {exe:'game.exe', class:'ignore'});
  assert.equal(proposedRule({name:'javaw.exe'}, 'game'), null);
  assert.equal(proposedRule({name:'javaw.exe', path:'C:\\Java\\javaw.exe'}, 'game'), null);
  assert.equal(proposedRule({name:'x.exe'}, 'launcher'), null);
});
test('duplicates ignore case and separator differences', () => {
  assert.equal(sameMatcher({path:'C:\\Games\\x.exe'}, {path:'c:/games/X.exe'}), true);
  assert.equal(sameMatcher({exe:'x.exe'}, {path:'c:/games/x.exe'}), false);
});
test('old or unavailable GPU samples cannot be acted on', () => {
  const now = Date.parse('2025-01-01T00:00:10Z');
  assert.equal(snapshotFresh({sampled_at:'2025-01-01T00:00:05Z', confidence:'high'}, now), true);
  assert.equal(snapshotFresh({sampled_at:'2025-01-01T00:00:00Z', confidence:'none'}, now), false);
  assert.equal(snapshotFresh({sampled_at:'2024-12-31T23:59:59Z', confidence:'high'}, now), false);
});
