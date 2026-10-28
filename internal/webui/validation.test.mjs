import { test } from 'node:test';
import assert from 'node:assert/strict';
import { validationGeneration } from './static/js/validation.js';

test('an edit or newer request makes older responses stale', () => {
  const revision = validationGeneration();
  const first = revision.next();
  const second = revision.next();
  assert.equal(revision.isCurrent(first), false);
  assert.equal(revision.isCurrent(second), true);
  revision.next();
  assert.equal(revision.isCurrent(second), false);
});
