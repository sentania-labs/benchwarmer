// Package simgpu is a development-only telemetry source that simulates the
// target GPU on machines without one. The managed runtime's processes (found
// by executable path) are given a loaded model's VRAM; competing load is
// injected by writing a small JSON file. It lets the full lifecycle run on
// Linux CI with a fake runtime. It is never used on the target.
package simgpu

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/signals"
	"github.com/sentania-labs/benchwarmer/internal/telemetry"
)

// Overrides are read from the control file on every sample.
type Overrides struct {
	ExternalVRAMMiB int      `json:"external_vram_mib"`
	ExternalUtilPct float64  `json:"external_util_pct"`
	TemperatureC    *float64 `json:"temperature_c,omitempty"`
	// Fail makes samples incomplete, simulating telemetry loss.
	Fail bool `json:"fail"`
}

// Source simulates a 16 GiB adapter.
type Source struct {
	mu          sync.Mutex
	controlFile string
	runtimeExe  string
	procs       func() ([]signals.Process, error)
	TotalMiB    int
	DesktopMiB  int
	ModelMiB    int
	ModelUtil   float64
}

// New returns a simulated source. runtimeExe identifies the runtime's
// processes; controlFile may be empty.
func New(controlFile, runtimeExe string, procs func() ([]signals.Process, error)) *Source {
	return &Source{controlFile: controlFile, runtimeExe: runtimeExe, procs: procs,
		TotalMiB: 16304, DesktopMiB: 900, ModelMiB: 12500, ModelUtil: 35}
}

const mib = 1 << 20

// Collect implements telemetry.Source.
func (s *Source) Collect() telemetry.Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var o Overrides
	if s.controlFile != "" {
		if b, err := os.ReadFile(s.controlFile); err == nil {
			_ = json.Unmarshal(b, &o)
		}
	}
	smp := telemetry.Sample{Time: now, AdapterLUID: "0x00000000_0x0000beef", AdapterName: "Simulated GPU",
		DedicatedTotalBytes: uint64(s.TotalMiB) * mib, HasAdapterInstances: true, Complete: true, TemperatureC: o.TemperatureC}
	if o.Fail {
		smp.Complete, smp.HasAdapterInstances = false, false
		smp.Errors = []string{"simulated telemetry failure"}
		return smp
	}
	const desktopPID, extPID = 1, 2
	eng3D, engCompute := "phys_0_eng_0", "phys_0_eng_1"
	used := s.DesktopMiB + o.ExternalVRAMMiB
	smp.Processes = append(smp.Processes, telemetry.ProcGPU{PID: desktopPID, Name: "dwm.exe", DedicatedBytes: uint64(s.DesktopMiB) * mib,
		EngineUtil: map[string]float64{"3D": 2}, EngineByKey: map[string]float64{eng3D: 2}})
	if o.ExternalVRAMMiB > 0 || o.ExternalUtilPct > 0 {
		smp.Processes = append(smp.Processes, telemetry.ProcGPU{PID: extPID, Name: "simulated-load", DedicatedBytes: uint64(o.ExternalVRAMMiB) * mib,
			EngineUtil: map[string]float64{"3D": o.ExternalUtilPct}, EngineByKey: map[string]float64{eng3D: o.ExternalUtilPct}})
	}
	ownUtil := 0.0
	if s.procs != nil {
		procs, _ := s.procs()
		for _, p := range procs {
			if p.Path != "" && samePath(p.Path, s.runtimeExe) {
				used += s.ModelMiB
				ownUtil = s.ModelUtil
				smp.Processes = append(smp.Processes, telemetry.ProcGPU{PID: p.PID, Name: p.Name, DedicatedBytes: uint64(s.ModelMiB) * mib,
					EngineUtil: map[string]float64{"Compute": s.ModelUtil}, EngineByKey: map[string]float64{engCompute: s.ModelUtil}})
				break
			}
		}
	}
	smp.Engines = []telemetry.Engine{
		{Key: eng3D, Type: "3D", UtilPct: min(100, 2+o.ExternalUtilPct)},
		{Key: engCompute, Type: "Compute", UtilPct: ownUtil},
	}
	smp.DedicatedUsedBytes = uint64(min(used, s.TotalMiB)) * mib
	return smp
}

func samePath(a, b string) bool {
	norm := func(p string) string { return strings.ToLower(filepath.Clean(strings.ReplaceAll(p, `\`, "/"))) }
	return norm(a) == norm(b)
}

// Rebuild implements telemetry.Source.
func (s *Source) Rebuild() error { return nil }

// Close implements telemetry.Source.
func (s *Source) Close() {}

var _ telemetry.Source = (*Source)(nil)
