// Package telemetry defines platform-neutral GPU telemetry samples and the
// logic that derives own-versus-external demand from them. Windows collectors
// live in wingpu_windows.go; tests and non-Windows builds use Fake.
package telemetry

import (
	"sort"
	"time"
)

// Sample is one observation of the target adapter.
type Sample struct {
	Time        time.Time `json:"time"`
	AdapterLUID string    `json:"adapter_luid,omitempty"`
	AdapterName string    `json:"adapter_name,omitempty"`

	DedicatedTotalBytes uint64 `json:"dedicated_total_bytes,omitempty"`
	// Adapter-wide dedicated usage from the GPU Adapter Memory counter.
	DedicatedUsedBytes uint64 `json:"dedicated_used_bytes"`
	SharedUsedBytes    uint64 `json:"shared_used_bytes"`
	// HasAdapterInstances is true when the adapter memory counter had at
	// least one instance for the target LUID. False means the memory figures
	// are unknown, not zero.
	HasAdapterInstances bool `json:"has_adapter_instances"`

	TemperatureC *float64 `json:"temperature_c,omitempty"`
	PowerPct     *float64 `json:"power_pct,omitempty"`
	FanRPM       *uint32  `json:"fan_rpm,omitempty"`

	// Engines holds per-engine utilization summed over processes, keyed by
	// engine key "phys_N_eng_M" with its engine type recorded.
	Engines   []Engine  `json:"engines,omitempty"`
	Processes []ProcGPU `json:"processes,omitempty"`

	// InvalidReadings counts engine readings discarded as impossible.
	InvalidReadings int `json:"invalid_readings,omitempty"`

	// CollectDuration is how long the collection itself took; GPU Engine
	// counters are known to be expensive on some systems.
	CollectDuration time.Duration `json:"collect_duration_ns"`
	// Complete is true when the counter-derived fields are real measurements
	// (primed, error-free, adapter present). Consumers must treat zeros in an
	// incomplete sample as unknown. Errors lists every failed source,
	// including temperature, which does not affect Complete.
	Complete bool     `json:"complete"`
	Errors   []string `json:"errors,omitempty"`
}

// Engine is one GPU engine's utilization summed over all processes.
type Engine struct {
	Key     string  `json:"key"`
	Type    string  `json:"type"`
	UtilPct float64 `json:"util_pct"`
}

// ProcGPU is one process's GPU usage on the target adapter.
type ProcGPU struct {
	PID            uint32             `json:"pid"`
	Name           string             `json:"name,omitempty"`
	EngineUtil     map[string]float64 `json:"engine_util,omitempty"` // by engine type, max engine of that type
	EngineByKey    map[string]float64 `json:"engine_by_key,omitempty"`
	DedicatedBytes uint64             `json:"dedicated_bytes"`
	SharedBytes    uint64             `json:"shared_bytes"`
}

// MaxUtil returns the process's highest utilization across engine types.
func (p ProcGPU) MaxUtil() float64 {
	m := 0.0
	for _, v := range p.EngineUtil {
		if v > m {
			m = v
		}
	}
	return m
}

// Demand splits a sample into the managed runtime's share and everything else.
type Demand struct {
	// Utilization is computed the way Task Manager does: per engine, sum the
	// processes using it; the adapter figure is the busiest engine.
	TotalUtilPct    float64 `json:"total_util_pct"`
	OwnUtilPct      float64 `json:"own_util_pct"`
	ExternalUtilPct float64 `json:"external_util_pct"`
	// ExternalUtilByType is the external busiest-engine figure per engine type
	// ("3D", "Compute", "Copy", "VideoDecode", ...).
	ExternalUtilByType map[string]float64 `json:"external_util_by_type,omitempty"`

	OwnDedicatedBytes      uint64 `json:"own_dedicated_bytes"`
	ExternalDedicatedBytes uint64 `json:"external_dedicated_bytes"`
	FreeDedicatedBytes     uint64 `json:"free_dedicated_bytes"`

	// Attributed is true when per-process data was present, so the own and
	// external split is measured rather than inferred.
	Attributed bool `json:"attributed"`
	// TopExternal lists the heaviest external processes, most demanding first.
	TopExternal []ProcGPU `json:"top_external,omitempty"`
}

// Split computes own vs. external demand given the managed runtime's PIDs.
func Split(s Sample, own map[uint32]bool) Demand {
	d := Demand{ExternalUtilByType: map[string]float64{}}
	type acc struct{ total, own float64 }
	perEngine := map[string]*acc{}
	engType := map[string]string{}
	for _, e := range s.Engines {
		engType[e.Key] = e.Type
		perEngine[e.Key] = &acc{}
	}
	var ext []ProcGPU
	for _, p := range s.Processes {
		isOwn := own[p.PID]
		for k, v := range p.EngineByKey {
			a := perEngine[k]
			if a == nil {
				a = &acc{}
				perEngine[k] = a
			}
			a.total += v
			if isOwn {
				a.own += v
			}
		}
		if isOwn {
			d.OwnDedicatedBytes += p.DedicatedBytes
		} else {
			d.ExternalDedicatedBytes += p.DedicatedBytes
			if p.DedicatedBytes > 0 || p.MaxUtil() > 0 {
				ext = append(ext, p)
			}
		}
	}
	d.Attributed = len(s.Processes) > 0
	for k, a := range perEngine {
		total := clampPct(a.total)
		ownU := clampPct(a.own)
		extU := clampPct(a.total - a.own)
		if !d.Attributed {
			// Only engine totals are known; external cannot be separated.
			for _, e := range s.Engines {
				if e.Key == k {
					total = clampPct(e.UtilPct)
				}
			}
			extU = total
		}
		d.TotalUtilPct = max(d.TotalUtilPct, total)
		d.OwnUtilPct = max(d.OwnUtilPct, ownU)
		d.ExternalUtilPct = max(d.ExternalUtilPct, extU)
		t := engType[k]
		if t == "" {
			t = "Unknown"
		}
		d.ExternalUtilByType[t] = max(d.ExternalUtilByType[t], extU)
	}
	if !d.Attributed {
		// Without per-process memory, adapter usage is all we have.
		d.ExternalDedicatedBytes = s.DedicatedUsedBytes
	} else if s.DedicatedUsedBytes > d.OwnDedicatedBytes {
		// Prefer the adapter-wide figure for external memory: per-process
		// counters can miss kernel and compositor allocations.
		d.ExternalDedicatedBytes = max(d.ExternalDedicatedBytes, s.DedicatedUsedBytes-d.OwnDedicatedBytes)
	}
	if s.DedicatedTotalBytes > s.DedicatedUsedBytes {
		d.FreeDedicatedBytes = s.DedicatedTotalBytes - s.DedicatedUsedBytes
	}
	sort.Slice(ext, func(i, j int) bool {
		if ext[i].MaxUtil() != ext[j].MaxUtil() {
			return ext[i].MaxUtil() > ext[j].MaxUtil()
		}
		return ext[i].DedicatedBytes > ext[j].DedicatedBytes
	})
	if len(ext) > 5 {
		ext = ext[:5]
	}
	d.TopExternal = ext
	return d
}

func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
