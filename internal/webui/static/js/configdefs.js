// Field definitions for the structured configuration forms. Paths and help
// text follow internal/config/config.go.

export const SECTIONS = [
  { key: "runtime", label: "Runtime", intro: "The managed llama-server process. Most changes here need a runtime reload." },
  { key: "network", label: "Network and security", intro: "Listeners and credentials. Changes here need a service restart." },
  { key: "profiles", label: "Profiles", intro: "Tunable policy for a period of time. Hard safety limits and critical contention live under Thresholds and always win." },
  { key: "thresholds", label: "Thresholds", intro: "Hard limits that apply in every profile and mode, and GPU sampling." },
  { key: "schedules", label: "Schedules", intro: "Which profile applies when. Outside every schedule the default profile applies." },
  { key: "applications", label: "Applications", intro: "How running processes are classified." },
  { key: "behavior", label: "Reload and recovery", intro: "How long Benchwarmer waits before loading again after trouble." },
  { key: "modes", label: "Manual modes", intro: "Duration choices for Pause AI and AI Priority." },
  { key: "system", label: "Retention and logging", intro: "Event history, logs, and metrics." },
  { key: "json", label: "Advanced JSON", intro: "Edit the whole configuration as JSON." },
];

const SECTION_OF = {
  runtime: "runtime", listen: "network", security: "network",
  profiles: "profiles", default_profile: "profiles", ai_priority_profile: "profiles",
  safety: "thresholds", contention: "thresholds", telemetry: "thresholds",
  schedules: "schedules", timezone: "schedules", applications: "applications",
  anti_thrash: "behavior", recovery: "behavior", signals: "behavior",
  modes: "modes", retention: "system", logging: "system", metrics: "system",
};

// sectionFor maps an API field path to the section that edits it.
export function sectionFor(path) {
  const head = String(path).split(/[.[]/)[0];
  return SECTION_OF[head] || "json";
}

const D = "duration";

function runtime() {
  return [
    { title: "Program and model", fields: [
      { path: "runtime.executable", label: "llama-server executable", help: "Absolute path, e.g. C:\\Benchwarmer\\llama\\llama-server.exe." },
      { path: "runtime.model_path", label: "Model file", help: "Absolute path to the .gguf model." },
      { path: "runtime.args", label: "Extra arguments", type: "lines",
        help: "One argument per line. Benchwarmer sets -m, --host, --port, -c and -ngl itself, so leave those out. Values shown as <redacted> are secrets: leave them unchanged to keep the stored value." },
      { path: "runtime.context_size", label: "Context size", type: "int", unit: "tokens", min: 256 },
      { path: "runtime.gpu_layers", label: "GPU layers", type: "int", min: 0, max: 9999, help: "Layers to offload to the GPU. 999 means all." },
    ] },
    { title: "Private runtime listener", note: "llama-server listens only on loopback; clients reach it through the Benchwarmer proxy.", fields: [
      { path: "runtime.host", label: "Host", help: "Must be a loopback address such as 127.0.0.1." },
      { path: "runtime.port", label: "Port", type: "int", min: 1, max: 65535 },
    ] },
    { title: "Process handling", note: "These apply live without a reload.", fields: [
      { path: "runtime.stop_mode", label: "How to stop the runtime", type: "select", options: [["graceful", "Graceful (Ctrl+C, kill only if it hangs)"], ["kill", "Hard kill"]],
        help: "Graceful is strongly recommended: on AMD hardware a hard kill of a long-running runtime can hang the GPU driver." },
      { path: "runtime.graceful_stop_timeout", label: "Graceful stop timeout", type: D, help: "How long to wait after Ctrl+C before a hard kill." },
      { path: "runtime.load_timeout", label: "Load timeout", type: D, help: "How long a model load may take before it counts as failed." },
      { path: "runtime.required_free_vram_mib", label: "Required free VRAM before loading", type: "int", unit: "MiB" },
      { path: "runtime.kill_verify_timeout", label: "Kill verify timeout", type: D, help: "How long to wait for the process tree to exit after termination." },
      { path: "runtime.vram_release_timeout", label: "VRAM release timeout", type: D, help: "How long to wait for the GPU to report the memory freed." },
      { path: "runtime.vram_release_tolerance_mib", label: "VRAM release tolerance", type: "int", unit: "MiB" },
      { path: "runtime.max_request_duration", label: "Maximum request duration", type: D, help: "Any single inference request is cut off after this long." },
      { path: "runtime.diagnostics_tail_bytes", label: "Diagnostics tail", type: "int", unit: "bytes", help: "How much runtime output to keep for troubleshooting." },
    ] },
  ];
}

function network() {
  return [
    { title: "Listeners", note: "host:port. An empty host listens on all interfaces.", fields: [
      { path: "listen.inference", label: "Inference (OpenAI-compatible /v1)", help: "The only listener meant for LAN exposure." },
      { path: "listen.management", label: "Management (this UI, /api/v1, /metrics)", help: "Keep on 127.0.0.1 unless you need remote management; remote access always needs the token." },
    ] },
    { title: "Inference HTTPS", note: "Required when the inference listener is not on loopback. Use a certificate from your CA: a .pfx exported with its private key, or PEM certificate and key files. Relative paths are inside the data folder. A replaced certificate file is picked up automatically.", fields: [
      { path: "listen.inference_tls.enabled", label: "Serve inference over HTTPS", type: "bool" },
      { path: "listen.inference_tls.cert_file", label: "Certificate file", help: "PEM chain (server certificate first) or .pfx / .p12." },
      { path: "listen.inference_tls.key_file", label: "Private key file (PEM only)", help: "Leave empty for a .pfx." },
      { path: "listen.inference_tls.pfx_password_file", label: "PFX password file", help: "A file containing the password, not the password itself." },
      { path: "listen.inference_tls.self_signed", label: "Use a generated self-signed certificate (testing only)", type: "bool",
        help: "Used only when no certificate file is set. Clients will not trust it unless told to." },
      { path: "listen.inference_tls.self_signed_hosts", label: "Extra names for the self-signed certificate", type: "lines", help: "One DNS name or IP per line." },
    ] },
    { title: "Authentication", note: "Token values are never shown or edited here. These are file locations on the PC; rotate a token by replacing its file.", fields: [
      { path: "security.loopback_trust", label: "Allow read-only access from this PC without a token", type: "bool",
        help: "Changes always need the management token, even from this PC." },
      { path: "security.management_token_file", label: "Management token file" },
      { path: "security.inference_token_file", label: "Inference token file" },
      { path: "security.agent_token_file", label: "Session agent token file" },
      { path: "security.require_inference_token", label: "Require the inference token from loopback clients too", type: "bool" },
    ] },
  ];
}

function profile(p) {
  return [
    { path: p + ".description", label: "Description" },
    { path: p + ".grace", label: "Grace", type: D, help: "How long an active request may continue once draining starts." },
    { path: p + ".grace_under_pressure", label: "Grace under pressure", type: D, help: "Replaces grace when soft external pressure is present during the drain. At most the grace." },
    { path: p + ".cooldown", label: "Cooldown", type: D, help: "Wait after the last competing workload was seen before loading again." },
    { path: p + ".external_util_soft_pct", label: "Soft external GPU use", type: "float", unit: "%", help: "Must be below the critical external GPU use." },
    { path: p + ".soft_window", label: "Soft window", type: D, help: "How long soft pressure must last before it counts." },
    { path: p + ".external_vram_soft_mib", label: "Soft external VRAM", type: "int", unit: "MiB", help: "Must be below the critical external VRAM." },
    { path: p + ".drain_on_game_process", label: "Drain as soon as a game process starts", type: "bool", help: "Early warning, before foreground or GPU use confirms it." },
    { path: p + ".drain_on_ambiguous", label: "Drain on ambiguous signals", type: "bool", help: "An unclassified fullscreen app, or a GPU-using child of a launcher, without GPU corroboration." },
    { path: p + ".drain_on_soft_contention", label: "Drain on soft contention", type: "bool", help: "When soft GPU or VRAM thresholds are exceeded for the soft window." },
    { path: p + ".game_confirm_util_pct", label: "Game confirmation GPU use", type: "float", unit: "%", help: "A game process using at least this much counts as confirmed gaming." },
    { path: p + ".game_confirm_vram_mib", label: "Game confirmation VRAM", type: "int", unit: "MiB" },
  ];
}

function thresholds() {
  return [
    { title: "Safety", note: "Crossing these terminates the runtime immediately in every mode, including AI Priority.", fields: [
      { path: "safety.gpu_temp_critical_c", label: "Critical GPU temperature", type: "float", unit: "\u00b0C" },
      { path: "safety.gpu_temp_resume_c", label: "Resume below", type: "float", unit: "\u00b0C", help: "Must be below the critical temperature." },
    ] },
    { title: "Critical contention", note: "External demand at these levels bypasses grace in every profile and mode.", fields: [
      { path: "contention.vram_free_critical_mib", label: "Free VRAM below", type: "int", unit: "MiB" },
      { path: "contention.external_vram_critical_mib", label: "External VRAM above", type: "int", unit: "MiB" },
      { path: "contention.external_util_critical_pct", label: "External GPU use above", type: "float", unit: "%" },
      { path: "contention.critical_window", label: "Critical window", type: D, help: "How long a critical level must last." },
      { path: "contention.clear_window", label: "Clear window", type: D, help: "How long signals must stay below soft thresholds before contention counts as gone." },
    ] },
    { title: "GPU telemetry", fields: [
      { path: "telemetry.adapter", label: "Adapter", placeholder: "automatic", help: "LUID or name substring. Empty picks the discrete GPU with the most memory. Changing it needs a service restart." },
      { path: "telemetry.sample_interval", label: "Sample interval", type: D },
      { path: "telemetry.loss_grace", label: "Telemetry loss grace", type: D, help: "How long telemetry may be missing before Benchwarmer yields the GPU." },
      { path: "telemetry.degraded_margin_pct", label: "Degraded margin", type: "int", unit: "%", help: "Soft thresholds tighten by this much when per-process attribution is unavailable." },
    ] },
  ];
}

function behavior() {
  return [
    { title: "Anti-thrashing", note: "Stops reload loops after repeated preemptions.", fields: [
      { path: "anti_thrash.window", label: "Window", type: D },
      { path: "anti_thrash.max_preemptions", label: "Preemptions allowed in the window", type: "int", min: 1 },
      { path: "anti_thrash.suppress_for", label: "Suppress reloads for", type: D },
    ] },
    { title: "Recovery", fields: [
      { path: "recovery.startup_cooldown", label: "After service start", type: D },
      { path: "recovery.resume_cooldown", label: "After resume from sleep", type: D },
      { path: "recovery.crash_backoff_initial", label: "First crash backoff", type: D },
      { path: "recovery.crash_backoff_max", label: "Maximum crash backoff", type: D },
      { path: "recovery.crash_reset_after", label: "Reset crash backoff after running", type: D },
      { path: "recovery.telemetry_recovery_cooldown", label: "After telemetry returns", type: D },
      { path: "recovery.device_lost_cooldown", label: "After GPU device loss", type: D },
    ] },
    { title: "Signals", fields: [
      { path: "signals.session_stale_after", label: "Session agent data stale after", type: D },
      { path: "signals.process_interval", label: "Process scan interval", type: D },
    ] },
  ];
}

function modes() {
  return [
    { title: "Duration choices", note: "Comma-separated, for example: 30m, 1h, 2h.", fields: [
      { path: "modes.pause_durations", label: "Pause AI durations", type: "durlist", placeholder: "30m, 1h, 2h, 4h" },
      { path: "modes.ai_priority_durations", label: "AI Priority durations", type: "durlist", placeholder: "30m, 1h, 2h" },
      { path: "modes.max_ai_priority", label: "Longest AI Priority allowed", type: D, help: "Caps every AI Priority request." },
    ] },
  ];
}

function system() {
  return [
    { title: "Retention", fields: [
      { path: "retention.events_days", label: "Keep events for", type: "int", unit: "days" },
      { path: "retention.events_max", label: "Maximum events kept", type: "int" },
      { path: "retention.log_max_mb", label: "Log file size", type: "int", unit: "MB" },
      { path: "retention.log_files", label: "Log files kept", type: "int" },
    ] },
    { title: "Logging", note: "Changes need a service restart.", fields: [
      { path: "logging.level", label: "Level", type: "select", options: [["debug", "Debug"], ["info", "Info"], ["warn", "Warning"], ["error", "Error"]] },
      { path: "logging.dir", label: "Log folder", placeholder: "data directory", help: "Empty uses the data directory." },
    ] },
    { title: "Metrics", fields: [
      { path: "metrics.enabled", label: "Serve Prometheus metrics at /metrics", type: "bool" },
    ] },
  ];
}

// sectionDefs returns field groups for a section, or for "profile" the
// fields of one profile under prefix.
export function sectionDefs(key, prefix) {
  switch (key) {
    case "runtime": return runtime();
    case "network": return network();
    case "profile": return profile(prefix);
    case "thresholds": return thresholds();
    case "behavior": return behavior();
    case "modes": return modes();
    case "system": return system();
  }
  return [];
}
