// The dashboard's plain-language reading of GET api/v1/status, for the person
// at the PC. The service classifies (status.owner: kind, severity, cause and
// the facts behind them); this module only words it, in the browser's local
// time. Pure, so node can test it.
import { clockTime, humanDuration, describeDurationChoice, parseGoDuration, parseTime } from "./format.js";

export const BADGES = {
  ok: "AI is on",
  wait: "Waiting",
  paused: "Paused",
  attention: "Needs attention",
  problem: "AI is off",
  unknown: "Not sure",
};

const SEVERITIES = Object.keys(BADGES);

// ownerView returns what the status head shows:
//   severity  one of BADGES' keys, drives color, icon and badge text
//   badge     short label
//   headline  one sentence saying what is going on and why
//   lines     further sentences (when AI returns, what to do); may be empty
//   action    { label, href } or null
//   key       changes only on a meaningful transition, never on a clock tick
//             or a temperature wobble, so a screen reader hears it once
export function ownerView(st, now = new Date()) {
  const o = st && st.owner;
  if (!o || !SEVERITIES.includes(o.severity)) return unknown();
  const at = (t) => clockTime(parseTime(t), now);
  const v = describe(o, st, at);
  if (!v) return unknown();
  const lines = v.lines || [];
  const mode = st.mode || {};
  if (o.kind !== "paused" && mode.mode === "pause") {
    lines.push("AI is also paused" + (parseTime(mode.until) ? " until " + at(mode.until) : " until you turn it back on") + ".");
  } else if (mode.mode === "ai_priority") {
    lines.push("AI first is on" + (parseTime(mode.until) ? " until " + at(mode.until) : "") + ".");
  }
  if (o.config_fallback) lines.push("A settings problem needs fixing; see the red box below.");
  return {
    severity: o.severity,
    badge: BADGES[o.severity],
    headline: v.headline,
    lines,
    action: v.action || null,
    key: [o.kind, o.severity, o.cause, o.after || "", o.yielding ? "y" : "", o.config_fallback ? "c" : "", mode.mode || ""].join("|"),
  };
}

function unknown() {
  return {
    severity: "unknown",
    badge: BADGES.unknown,
    headline: "Benchwarmer can't tell what AI is doing right now.",
    lines: ["Open the details for troubleshooting below."],
    action: null,
    key: "unknown",
  };
}

function backAround(o, at, qualifier) {
  return o.return_at ? ["AI should be back around " + at(o.return_at) + ", " + qualifier + "."] : [];
}

function afterStops(o, what) {
  const d = o.resume_delay_seconds ? humanDuration(o.resume_delay_seconds * 1000) : "";
  return [d ? "AI comes back about " + d + " after " + what + ", if the GPU is free." : "AI comes back after " + what + "."];
}

function describe(o, st, at) {
  const stepping = o.yielding ? "AI is stepping aside because " : "AI is off because ";
  const app = o.app ? " (" + o.app + ")" : "";
  switch (o.cause) {
    case "ready":
      return { headline: "AI is on and ready to use." };
    case "busy":
      return { headline: "AI is on and working on a request." };
    case "loading":
      return { headline: "AI is starting up. It will be ready in a moment." };
    case "starting":
      return { headline: "AI is about to start loading." };
    case "game":
      return { headline: stepping + "a game" + app + " is using the GPU.", lines: afterStops(o, "the game stops") };
    case "gpu_busy":
      return { headline: stepping + (o.app || "something else") + " is using the GPU heavily.", lines: afterStops(o, "that stops") };
    case "cooldown": {
      const why = { game: "A game was using the GPU", gpu_busy: "Something was using the GPU heavily", heat: "The GPU was too hot" }[o.after];
      return {
        headline: (why ? why + ", so AI" : "AI") + " is taking a short break before it comes back.",
        lines: backAround(o, at, "if the GPU stays free"),
      };
    }
    case "startup":
      return { headline: "Benchwarmer just started and is giving the GPU a moment before it loads AI.", lines: backAround(o, at, "if the GPU is free") };
    case "resume":
      return { headline: "The PC just woke up. AI is waiting a moment before it loads.", lines: backAround(o, at, "if the GPU is free") };
    case "drained":
      return { headline: "AI was unloaded on request and is waiting a few minutes before it loads again.", lines: backAround(o, at, "if the GPU is free") };
    case "recovering":
      return { headline: "AI is waiting a short while before it loads again.", lines: backAround(o, at, "if the GPU is free") };
    case "telemetry_recovered":
      return { headline: "GPU readings are back. AI is waiting a moment before it loads again.", lines: backAround(o, at, "if the GPU is free") };
    case "suppressed":
      return {
        headline: "AI was pushed off the GPU several times in a row, so it is staying off for a while instead of restarting over and over.",
        lines: backAround(o, at, "if the GPU is free"),
      };
    case "low_vram":
      return { headline: "There is not enough free GPU memory to load AI right now.", lines: ["It loads on its own when another program frees some up."] };
    case "vram_too_small":
      return {
        headline: "This GPU does not have enough memory for the AI model as configured, so AI can never load.",
        lines: ["Lower the required free memory or choose a smaller model."],
        action: { label: "Open runtime settings", href: "#/config/runtime" },
      };
    case "stopping":
      return { headline: "Benchwarmer is shutting down." };
    case "sleeping":
      return { headline: "The PC is going to sleep, so AI is stopping." };
    case "paused": {
      const until = parseTime(st.mode && st.mode.until);
      const back = parseTime(o.return_at);
      const lines = [];
      if (until && back && back.getTime() > until.getTime()) {
        lines.push("AI can start again around " + at(o.return_at) + ", if the GPU is free by then.");
      } else if (until) {
        lines.push("Then AI starts again, if the GPU is free.");
      } else {
        lines.push("Choose Back to normal to turn it on again.");
      }
      return {
        headline: until ? "AI is paused until " + at(st.mode.until) + "." : "AI is paused until you turn it back on.",
        lines,
      };
    }
    case "too_hot": {
      const t = st.gpu && st.gpu.temperature_c;
      return {
        headline: "The GPU is too hot" + (Number.isFinite(t) ? " (" + Math.round(t) + " °C)" : "") + ", so AI is off to protect it.",
        lines: ["Check the PC's fans and airflow. AI starts again once the GPU cools down."],
      };
    }
    case "setup": {
      const d = (o.detail || "").trim();
      return {
        headline: "AI can't start until setup is finished" + (d ? ": " + d : "") + ".",
        action: { label: "Choose a model", href: "#/config/runtime" },
      };
    }
    case "crash":
      return {
        headline: "AI stopped unexpectedly.",
        lines: [o.return_at ? "Benchwarmer will try again around " + at(o.return_at) + "." : "Benchwarmer will try again shortly."],
        action: { label: "See what happened", href: "#/events" },
      };
    case "gpu_fault":
      return {
        headline: "The GPU had a problem (a driver reset, or it went missing), so AI is off until it recovers.",
        lines: o.return_at ? ["Benchwarmer will try again around " + at(o.return_at) + "."] : [],
        action: { label: "See what happened", href: "#/events" },
      };
    case "no_gpu_readings":
      return { headline: "Benchwarmer can't read the GPU, so AI stays off to be safe until it can.", action: { label: "See what happened", href: "#/events" } };
    case "no_process_list":
      return { headline: "Benchwarmer can't see which programs are running, so it can't rule out a game. AI stays off until it can." };
  }
  return null; // a cause this page does not know is unknown, not a guess
}

// quickChoices lists the one-click buttons for a mode: up to three of the
// configured durations (modes.*_durations), then, for Pause, "Until
// tomorrow" labelled with the time the service will really end it. Each is
// { label, duration }.
export function quickChoices(mode, options, untilTomorrowAt, now = new Date()) {
  const o = options || {};
  const timed = (mode === "pause" ? o.pause_durations : o.ai_priority_durations) || [];
  const out = timed.slice(0, 3).map((d) => ({ label: describeDurationChoice(String(d)), duration: String(d) }));
  if (mode === "pause" && parseTime(untilTomorrowAt)) {
    out.push({ label: describeDurationChoice("until_tomorrow", untilTomorrowAt, now), duration: "until_tomorrow" });
  }
  return out;
}

// appliedText is the confirmation after a quick mode change.
export function appliedText(mode, duration, untilTomorrowAt, now = new Date()) {
  if (mode === "auto") return "Back to normal.";
  const what = mode === "pause" ? "AI paused" : "AI first on";
  if (duration === "until_tomorrow") {
    const t = mode === "pause" ? parseTime(untilTomorrowAt) : null;
    return what + (t ? " until " + clockTime(t, now) : " until tomorrow") + ".";
  }
  if (duration === "until_reboot") return what + " until the PC restarts.";
  if (!duration) return what + " until you turn it back on.";
  const ms = parseGoDuration(duration);
  return what + (ms == null ? "" : " for " + humanDuration(ms)) + ".";
}
