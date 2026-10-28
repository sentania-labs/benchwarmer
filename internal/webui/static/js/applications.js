// Rules are inserted first so they cannot be shadowed by an earlier matcher.
export function proposedRule(process, classification) {
  if (!process || !['game', 'ignore'].includes(classification)) return null;
  const name = (process.name || '').toLowerCase();
  if (name === 'javaw.exe') return null;
  if (process.path) return { path: process.path, class: classification };
  if (process.name) return { exe: process.name, class: classification };
  return null;
}
export function sameMatcher(a, b) {
  const norm = s => (s || '').toLowerCase().replaceAll('\\', '/');
  return ['path', 'exe'].some(k => a[k] && b[k] && norm(a[k]) === norm(b[k]));
}
export function snapshotFresh(gpu, now = Date.now()) {
  const sampled = Date.parse(gpu?.sampled_at);
  return Number.isFinite(sampled) && now - sampled >= -5000 && now - sampled <= 10000 && gpu.confidence !== 'none';
}
