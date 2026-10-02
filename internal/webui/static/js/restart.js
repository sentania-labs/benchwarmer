// A healthy response from the old process does not prove a restart happened.
// Compare identities so a restart shorter than one poll interval is visible.
export async function waitForRestart(previous, probe,
  sleep = ms => new Promise(resolve => setTimeout(resolve, ms)),
  now = () => Date.now(), timeout = 120000) {
  const started = now();
  while (now() - started < timeout) {
    try {
      const health = await probe(Math.min(5000, timeout - (now() - started)));
      if (now() - started < timeout && previous && health.instance_id && health.instance_id !== previous) return true;
    } catch {
      // The listener can disappear while the service is restarting.
    }
    await sleep(1000);
  }
  return false;
}
