// Tracks edits and requests so a response for an older draft is ignored.
export function validationGeneration() {
  let current = 0;
  return {
    next: () => ++current,
    isCurrent: (generation) => generation === current,
  };
}

// True when the running config came from a fallback rather than config.json.
export const isFallbackSource = (source) => !!source && source !== "primary";

// Save needs something to write: an edit, or a fallback config the owner is
// repairing into a new primary file. Either way the draft must have validated.
export function saveEnabled({ dirty, fallback, validated, pending, errorCount }) {
  return (dirty || fallback) && validated && !pending && errorCount === 0;
}
