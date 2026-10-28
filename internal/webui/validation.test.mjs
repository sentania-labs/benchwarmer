import { test } from 'node:test';
import assert from 'node:assert/strict';
import { validationGeneration, isFallbackSource, saveEnabled } from './static/js/validation.js';

test('an edit or newer request makes older responses stale', () => {
  const revision = validationGeneration();
  const first = revision.next();
  const second = revision.next();
  assert.equal(revision.isCurrent(first), false);
  assert.equal(revision.isCurrent(second), true);
  revision.next();
  assert.equal(revision.isCurrent(second), false);
});

const ok = { dirty: false, fallback: false, validated: true, pending: false, errorCount: 0 };

test('a primary config needs an edit before Save is enabled', () => {
  assert.equal(saveEnabled(ok), false);
  assert.equal(saveEnabled({ ...ok, dirty: true }), true);
});

test('a fallback config can be saved unchanged once it validates', () => {
  const fb = { ...ok, fallback: true };
  assert.equal(saveEnabled(fb), true);
  assert.equal(saveEnabled({ ...fb, validated: false }), false);
  assert.equal(saveEnabled({ ...fb, pending: true }), false);
  assert.equal(saveEnabled({ ...fb, errorCount: 1 }), false);
});

test('only last_good and default sources count as fallback', () => {
  assert.equal(isFallbackSource('primary'), false);
  assert.equal(isFallbackSource(''), false);
  assert.equal(isFallbackSource('last_good'), true);
  assert.equal(isFallbackSource('default'), true);
});
