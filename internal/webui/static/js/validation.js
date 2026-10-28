// Tracks edits and requests so a response for an older draft is ignored.
export function validationGeneration() {
  let current = 0;
  return {
    next: () => ++current,
    isCurrent: (generation) => generation === current,
  };
}
