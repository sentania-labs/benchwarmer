import { test } from 'node:test';
import { strict as assert } from 'node:assert';
import { capabilities, compatibility, compatibilityMessage } from './static/js/compatibility.js';

const fixture = { 'v0.0.0/b1': { testarch: 'compatible', oldarch: 'incompatible' } };
const model = { architecture: 'testarch' };
const runtime = { llama_cpp_tag: 'b1', ollama_version: 'v0.0.0' };

test('shipped capability table makes every result unknown', () => {
  assert.deepEqual(capabilities, {});
  assert.equal(compatibility(model, runtime), 'unknown');
  assert.match(compatibilityMessage(model, runtime), /unknown; you can still save/);
});

test('compatible, incompatible and unknown are distinct', () => {
  assert.equal(compatibility(model, runtime, fixture), 'compatible');
  assert.equal(compatibility({ architecture: 'oldarch' }, runtime, fixture), 'incompatible');
  assert.equal(compatibility({ architecture: 'other' }, runtime, fixture), 'unknown');
  assert.equal(compatibility(model, {}, fixture), 'unknown');
  assert.match(compatibilityMessage(model, runtime, fixture), /not verified/);
  assert.match(compatibilityMessage({ architecture: 'oldarch' }, runtime, fixture), /does not support/);
  assert.match(compatibilityMessage(null, null), /may block/);
});
