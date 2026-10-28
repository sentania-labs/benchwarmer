import { test } from 'node:test';
import assert from 'node:assert/strict';
import { modelOptions, proposalRows, parseEdit, applyRows, summary, inspectUrl, quantName, baseName } from './static/js/modelplan.js';

const models = [
  { name: 'a.gguf', path: 'C:\\ProgramData\\Benchwarmer\\models\\a.gguf', size_bytes: 4690 * 1048576 },
  { name: 'b.gguf', path: 'C:\\ProgramData\\Benchwarmer\\models\\b.gguf', size_bytes: 100 * 1048576 },
];

test('current model in the folder is selected, nothing added', () => {
  const r = modelOptions(models, 'c:/programdata/benchwarmer/models/A.GGUF');
  assert.equal(r.options.length, 2);
  assert.equal(r.selected, models[0].path);
  assert.equal(r.outside, false);
});
test('current model outside the folder is kept as its own option', () => {
  const r = modelOptions(models, 'D:\\old\\flint.gguf');
  assert.equal(r.options.length, 3);
  assert.equal(r.selected, 'D:\\old\\flint.gguf');
  assert.equal(r.outside, true);
  assert.match(r.options[0].label, /flint\.gguf \(not in the models folder\)/);
});
test('no model configured selects nothing', () => {
  const r = modelOptions(models, '');
  assert.equal(r.selected, '');
  assert.equal(r.options.length, 2);
  assert.equal(modelOptions(undefined, '').options.length, 0);
});

const ok = (recs) => ({ status: 'ok', plan: { recommendations: recs } });
const draft = () => ({ runtime: { context_size: 131072, required_free_vram_mib: 12500, args: ['-np', '2'] } });

test('rows carry before, after, reason and certainty', () => {
  const rows = proposalRows(ok([
    { path: 'runtime.context_size', value: 32768, reason: 'fits', certainty: 'estimate' },
    { path: 'runtime.required_free_vram_mib', value: 9000, reason: 'estimated', certainty: 'from_file' },
  ]), draft());
  assert.deepEqual(rows.map(r => [r.path, r.before, r.after, r.certainty]), [
    ['runtime.context_size', 131072, 32768, 'estimate'],
    ['runtime.required_free_vram_mib', 12500, 9000, 'from the model file'],
  ]);
});
test('unchanged, unknown-field and non-integer rows are dropped', () => {
  const rows = proposalRows(ok([
    { path: 'runtime.context_size', value: 131072, reason: 'same' },
    { path: 'runtime.args', value: 1, reason: 'not ours' },
    { path: 'runtime.required_free_vram_mib', value: 1.5, reason: 'bad' },
  ]), draft());
  assert.deepEqual(rows, []);
});
test('rows outside the server bounds are dropped', () => {
  const rows = proposalRows(ok([
    { path: 'runtime.context_size', value: 2000000, reason: 'too big' },
    { path: 'runtime.context_size', value: 100, reason: 'too small' },
  ]), draft());
  assert.deepEqual(rows, []);
});
test('failed inspections give no rows', () => {
  for (const status of ['not_found', 'unreadable', 'not_gguf', 'unsupported', 'invalid']) {
    assert.deepEqual(proposalRows({ status, plan: { recommendations: [{ path: 'runtime.context_size', value: 1, reason: '' }] } }, draft()), []);
  }
  assert.deepEqual(proposalRows(null, draft()), []);
});
test('parseEdit enforces whole numbers and server bounds', () => {
  assert.deepEqual(parseEdit('runtime.context_size', ' 32768 '), { value: 32768 });
  assert.ok(parseEdit('runtime.context_size', '100').error);
  assert.ok(parseEdit('runtime.context_size', '12.5').error);
  assert.ok(parseEdit('runtime.context_size', '').error);
  assert.ok(parseEdit('runtime.context_size', '-5').error);
  assert.ok(parseEdit('runtime.args', '5').error);
  assert.deepEqual(parseEdit('runtime.required_free_vram_mib', '0'), { value: 0 });
});
test('applyRows changes only accepted rows and nothing else', () => {
  const d = draft();
  const n = applyRows(d, [{ path: 'runtime.context_size', after: 32768 }, { path: 'runtime.args', after: 5 }]);
  assert.equal(n, 1);
  assert.deepEqual(d, { runtime: { context_size: 32768, required_free_vram_mib: 12500, args: ['-np', '2'] } });
  assert.equal(applyRows(d, []), 0);
});
test('summary lists known facts only', () => {
  const s = summary({ size_bytes: 4690 * 1048576, metadata: { architecture: 'llama', block_count: 32, context_length: 131072, file_type: 15, expert_count: 8, expert_used_count: 2 } });
  assert.deepEqual(s.map(x => x[0]), ['Architecture', 'Quantization', 'File size', 'Layers', 'Trained context', 'Experts']);
  assert.equal(s.find(x => x[0] === 'Quantization')[1], 'Q4_K_M');
  assert.equal(s.find(x => x[0] === 'File size')[1], '4.6 GiB');
  assert.deepEqual(summary({}), []);
});
test('helpers', () => {
  assert.equal(inspectUrl('C:\\m a.gguf', 8192), 'api/v1/setup/model?path=C%3A%5Cm%20a.gguf&context=8192');
  assert.equal(inspectUrl('x', 0), 'api/v1/setup/model?path=x');
  assert.equal(quantName(999), 'type 999');
  assert.equal(quantName(undefined), '');
  assert.equal(baseName('D:\\a\\b.gguf'), 'b.gguf');
});
