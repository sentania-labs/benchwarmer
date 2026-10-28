package config

import (
	"reflect"
	"sort"
)

// HistoryChange describes a change using redacted JSON values. A missing
// value is represented by nil. Arrays are compared and reported as a whole.
type HistoryChange struct {
	Path   string `json:"path"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

// HistoryDiff compares the original values so a secret-only edit is still
// visible, but emits only redacted values. Never serialize the inputs.
func HistoryDiff(before, after Config) []HistoryChange {
	originalBefore, originalAfter := sections(before), sections(after)
	safeBefore, safeAfter := sections(Redact(before)), sections(Redact(after))
	var changes []HistoryChange
	var walk func(string, any, any, any, any)
	walk = func(path string, old, next, safeOld, safeNext any) {
		if reflect.DeepEqual(old, next) {
			return
		}
		om, ook := old.(map[string]any)
		nm, nok := next.(map[string]any)
		if ook && nok {
			so, _ := safeOld.(map[string]any)
			sn, _ := safeNext.(map[string]any)
			keys := make(map[string]bool, len(om)+len(nm))
			for k := range om {
				keys[k] = true
			}
			for k := range nm {
				keys[k] = true
			}
			sorted := make([]string, 0, len(keys))
			for k := range keys {
				sorted = append(sorted, k)
			}
			sort.Strings(sorted)
			for _, k := range sorted {
				child := k
				if path != "" {
					child = path + "." + k
				}
				walk(child, om[k], nm[k], so[k], sn[k])
			}
			return
		}
		if path == "runtime.args" {
			safeOld, safeNext = Redacted, Redacted
		}
		changes = append(changes, HistoryChange{Path: path, Before: safeOld, After: safeNext})
	}
	walk("", originalBefore, originalAfter, safeBefore, safeAfter)
	return changes
}
