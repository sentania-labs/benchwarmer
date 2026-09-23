//go:build windows

package main

import (
	"github.com/sentania-labs/benchwarmer/internal/telemetry"
)

type gpuSource interface {
	Collect() telemetry.Sample
	Close()
}

func newGPUSource(selector string) (gpuSource, error) {
	c, err := telemetry.NewWindowsCollector(selector)
	if err != nil {
		// Return an untyped nil: a nil *WindowsCollector in the interface
		// would pass callers' nil checks and crash on use.
		return nil, err
	}
	return c, nil
}

func listAdapters() (any, error) {
	ads, err := telemetry.Adapters()
	if err != nil {
		return nil, err
	}
	type withPerf struct {
		telemetry.Adapter
		Perf    *telemetry.PerfData `json:"perf,omitempty"`
		PerfErr string              `json:"perf_error,omitempty"`
	}
	var out []withPerf
	for _, a := range ads {
		w := withPerf{Adapter: a}
		if pd, err := telemetry.QueryPerfData(a); err == nil {
			w.Perf = &pd
		} else {
			w.PerfErr = err.Error()
		}
		out = append(out, w)
	}
	return out, nil
}
