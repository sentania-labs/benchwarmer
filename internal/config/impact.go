package config

import (
	"encoding/json"
	"reflect"
	"sort"
)

// Impact says what applying a config change requires.
type Impact struct {
	// Changed lists top-level sections that differ.
	Changed []string `json:"changed"`
	// RuntimeReload: the managed runtime must be restarted to take effect.
	RuntimeReload bool `json:"runtime_reload"`
	// ServiceRestart: the service must restart (listeners, credentials).
	ServiceRestart bool `json:"service_restart"`
	// Sections needing each action, for display.
	ReloadSections  []string `json:"reload_sections,omitempty"`
	RestartSections []string `json:"restart_sections,omitempty"`
}

// Runtime fields that affect only Benchwarmer's own handling of the runtime
// apply live; the rest change the process and need a reload.
var liveRuntimeFields = map[string]bool{
	"load_timeout": true, "required_free_vram_mib": true, "kill_verify_timeout": true,
	"vram_release_timeout": true, "vram_release_tolerance_mib": true,
	"max_request_duration": true, "diagnostics_tail_bytes": true,
}

// Classify compares two configs.
func Classify(old, new Config) Impact {
	var im Impact
	o, n := sections(old), sections(new)
	keys := make([]string, 0, len(n))
	for k := range n {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if reflect.DeepEqual(o[k], n[k]) {
			continue
		}
		im.Changed = append(im.Changed, k)
		switch k {
		case "runtime":
			if runtimeNeedsReload(old.Runtime, new.Runtime) {
				im.RuntimeReload = true
				im.ReloadSections = append(im.ReloadSections, k)
			}
		case "listen", "security", "logging":
			im.ServiceRestart = true
			im.RestartSections = append(im.RestartSections, k)
		case "telemetry":
			if old.Telemetry.Adapter != new.Telemetry.Adapter {
				im.ServiceRestart = true
				im.RestartSections = append(im.RestartSections, k)
			}
		}
	}
	return im
}

func runtimeNeedsReload(a, b Runtime) bool {
	am, bm := fields(a), fields(b)
	for k, v := range bm {
		if !liveRuntimeFields[k] && !reflect.DeepEqual(am[k], v) {
			return true
		}
	}
	return false
}

func sections(c Config) map[string]any { return fields(c) }

func fields(v any) map[string]any {
	b, _ := json.Marshal(v)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	return m
}
