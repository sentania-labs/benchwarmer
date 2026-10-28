import { test } from 'node:test';
import { strict as assert } from 'node:assert';
import { compatibility, compatibilityMessage } from './static/js/compatibility.js';

test('only a known architecture and identified runtime give a compatibility result', () => {
  const model = { architecture: 'flint' };
  const runtime = { llama_cpp_tag: 'b11901', ollama_version: 'v0.40.12' };
  assert.equal(compatibility(model, runtime), 'compatible');
  assert.equal(compatibility(model, {}), 'unknown');
  assert.equal(compatibility({ architecture: 'other' }, runtime), 'unknown');
  assert.match(compatibilityMessage(model, runtime), /not verified/);
  assert.match(compatibilityMessage(null, null), /may block/);
});
