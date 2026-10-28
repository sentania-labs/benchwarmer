// Pure helpers for the dashboard's mode controls, kept free of the DOM so
// node can test them.

const FIXED_DURATIONS = ["until_tomorrow", "until_reboot"];

// normalizeOptions turns status.mode_options into plain string lists.
export function normalizeOptions(o) {
  o = o || {};
  return { pause: (o.pause_durations || []).map(String), ai_priority: (o.ai_priority_durations || []).map(String) };
}

// durationChoices lists the values for the "For" select: the configured
// durations (modes.*_durations), then the fixed ones. Pause may also run
// indefinitely ("").
export function durationChoices(mode, options) {
  const timed = (mode === "pause" ? options.pause : options.ai_priority) || [];
  return mode === "pause" ? [...timed, ...FIXED_DURATIONS, ""] : [...timed, ...FIXED_DURATIONS];
}

// modeKey identifies the mode the service reports, so a change made
// elsewhere (the tray, an expired pause) can be told from "no news".
export function modeKey(m) {
  m = m || {};
  return JSON.stringify([m.mode || "auto", m.until || null]);
}

// followsServer says whether the form should take the service's mode: only
// when it differs from the last one adopted and the visitor has no unsent
// change in the form.
export function followsServer(dirty, syncedKey, key) {
  return !dirty && key !== syncedKey;
}
