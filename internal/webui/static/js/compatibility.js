// A capability entry means architecture recognition, not that every
// quantization or hardware combination will run. Add only verified evidence.
export const capabilities = {};

export function compatibility(model, runtime, table = capabilities) {
  if (!model?.architecture || !runtime?.llama_cpp_tag) return "unknown";
  return table[runtime.ollama_version + "/" + runtime.llama_cpp_tag]?.[model.architecture] || "unknown";
}

export function compatibilityMessage(model, runtime, table = capabilities) {
  const state = compatibility(model, runtime, table);
  const arch = model?.architecture ? "Model architecture: " + model.architecture + ". " : "Model architecture is unknown. ";
  const build = runtime?.llama_cpp_tag ? "Verified installed runtime: Ollama " + runtime.ollama_version + ", llama.cpp " + runtime.llama_cpp_tag + ". " : "Runtime build identity is unknown. ";
  const result = state === "compatible" ? "This runtime recognizes the model architecture; model format and hardware support are not verified. " :
    state === "incompatible" ? "This runtime does not support the model architecture. " :
    "Compatibility is unknown; you can still save. ";
  return result + arch + build + "Windows security policy, including Defender ASR, may block access at a given path. A path or signature does not guarantee access.";
}
