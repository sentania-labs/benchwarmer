import { test } from 'node:test';
import assert from 'node:assert/strict';
import { refreshProfilePreviews } from './static/js/config.js';

test('profile edits refresh every affected preview without changing the draft', () => {
  const profile = (grace, drain) => ({ description: 'Profile', grace, grace_under_pressure: '2s', cooldown: '10s',
    drain_on_game_process: drain, drain_on_ambiguous: false, drain_on_soft_contention: true,
    external_util_soft_pct: 20, external_vram_soft_mib: 100, soft_window: '1s',
    game_confirm_util_pct: 10, game_confirm_vram_mib: 20 });
  const draft = { default_profile: 'one', ai_priority_profile: 'one', schedules: [{ profile: 'two' }],
    profiles: { one: profile('5s', true), two: profile('9s', false) } };
  const previews = ['default_profile', 'ai_priority_profile', 'schedules[0].profile'].map(path => ({ dataset: { profilePreview: path }, textContent: '' }));
  const root = { querySelectorAll: () => previews };
  refreshProfilePreviews(draft, root);
  draft.profiles.one.grace = '8s';
  draft.profiles.one.drain_on_game_process = false;
  const before = JSON.stringify(draft);
  refreshProfilePreviews(draft, root);
  for (const p of previews.slice(0, 2)) {
    assert.match(p.textContent, /Grace 8s/);
    assert.match(p.textContent, /Drain on game process: no/);
  }
  assert.match(previews[2].textContent, /Grace 9s/);
  assert.equal(JSON.stringify(draft), before);
  draft.schedules[0].profile = 'one';
  refreshProfilePreviews(draft, root);
  assert.match(previews[2].textContent, /Grace 8s/);
});
