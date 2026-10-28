import { test } from 'node:test';
import assert from 'node:assert/strict';
import { proposedRule, sameMatcher, snapshotFresh, searchableValues } from './static/js/applications.js';

test('staging uses exact paths and never makes a name-only javaw.exe rule', () => {
  assert.deepEqual(proposedRule({name:'game.exe', path:'C:\\Games\\game.exe'}, 'game'), {path:'C:\\Games\\game.exe', class:'game'});
  assert.deepEqual(proposedRule({name:'game.exe'}, 'ignore'), {exe:'game.exe', class:'ignore'});
  assert.equal(proposedRule({name:'javaw.exe'}, 'game'), null);
  assert.equal(proposedRule({name:'javaw.exe'}, 'ignore'), null);
  assert.deepEqual(proposedRule({name:'javaw.exe', path:'C:\\Java\\javaw.exe'}, 'game'), {path:'C:\\Java\\javaw.exe', class:'game'});
  assert.deepEqual(proposedRule({name:'JAVAW.EXE', path:'C:\\Java\\javaw.exe'}, 'ignore'), {path:'C:\\Java\\javaw.exe', class:'ignore'});
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

test('search finds literal Windows paths in rules and observed processes', () => {
  const path = 'C:\\Games\\game.exe';
  const query = 'c:\\games\\game.exe';
  assert.equal(searchableValues({path, class:'game'}).includes(query), true);
  assert.equal(searchableValues({name:'game.exe', path, pid:42}).includes(query), true);
  assert.equal(searchableValues({path}).includes('other.exe'), false);
});
