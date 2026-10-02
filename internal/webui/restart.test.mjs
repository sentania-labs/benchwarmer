import { test } from 'node:test';
import assert from 'node:assert/strict';
import { waitForRestart } from './static/js/restart.js';

async function poll(sequence) {
  let time = 0, calls = 0;
  const probe = async () => {
    const value = sequence[Math.min(calls++, sequence.length - 1)];
    if (value instanceof Error) throw value;
    return value;
  };
  const result = await waitForRestart('old', probe, async ms => { time += ms; }, () => time, 3000);
  return { result, calls };
}

test('fast restart succeeds without an observed outage', async () => {
  assert.equal((await poll([{instance_id:'new'}])).result, true);
});
test('old instance never proves restart', async () => {
  assert.equal((await poll([{instance_id:'old'}])).result, false);
});
test('outage followed by old instance is not success', async () => {
  assert.equal((await poll([new Error('offline'), {instance_id:'old'}])).result, false);
});
test('slow restart waits through outage', async () => {
  assert.deepEqual(await poll([{instance_id:'old'},new Error('offline'),{instance_id:'new'}]),{result:true,calls:3});
});
test('a service that never returns times out', async () => {
  assert.equal((await poll([new Error('offline')])).result, false);
});
test('missing identity does not prove restart', async () => {
  assert.equal((await poll([{}])).result, false);
});
