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
test('freshness comes from the server-computed process scan age', () => {
  assert.equal(snapshotFresh({process_scan_age_ms: 5000, confidence: 'high'}), true);
  assert.equal(snapshotFresh({process_scan_age_ms: 5000, confidence: 'high'}, 6000), false);
  assert.equal(snapshotFresh({process_scan_age_ms: 11000, confidence: 'high'}), false);
  assert.equal(snapshotFresh({process_scan_age_ms: 0, confidence: 'none'}), false);
  assert.equal(snapshotFresh({sampled_at: new Date().toISOString(), confidence: 'high'}), false);
  assert.equal(snapshotFresh({}), false);
});
