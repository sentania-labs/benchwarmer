package observe

import (
	"strings"
	"testing"

	"github.com/sentania-labs/benchwarmer/internal/policy"
	"github.com/sentania-labs/benchwarmer/internal/telemetry"
)

func TestConfidenceDiagnostics(t *testing.T) {
	tests := []struct {
		name      string
		sample    *telemetry.Sample
		footprint int
		want      policy.Confidence
		reason    string
	}{
		{"missing", nil, 0, policy.ConfidenceNone, "missing or incomplete"},
		{"incomplete", &telemetry.Sample{}, 0, policy.ConfidenceNone, "missing or incomplete"},
		{"no processes", &telemetry.Sample{Complete: true}, 0, policy.ConfidenceDegraded, "instances absent"},
		{"runtime absent", &telemetry.Sample{Complete: true, Processes: []telemetry.ProcGPU{{PID: 200}}}, 0, policy.ConfidenceDegraded, "runtime PIDs absent"},
		{"zero memory", &telemetry.Sample{Complete: true, Processes: []telemetry.ProcGPU{{PID: 100}}}, 4000, policy.ConfidenceDegraded, "dedicated bytes zero"},
		{"mismatch", &telemetry.Sample{Complete: true, DedicatedUsedBytes: 8000 * mib, Processes: []telemetry.ProcGPU{{PID: 100, DedicatedBytes: 4000 * mib}}}, 4000, policy.ConfidenceDegraded, "adapter=8000.0 MiB processes=4000.0 MiB"},
		{"healthy", &telemetry.Sample{Complete: true, DedicatedUsedBytes: 4000 * mib, Processes: []telemetry.ProcGPU{{PID: 100, DedicatedBytes: 4000 * mib}}}, 4000, policy.ConfidenceHigh, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conf, reasons := confidence(Input{Sample: tt.sample, FootprintMiB: tt.footprint}, map[uint32]bool{100: true})
			if conf != tt.want || !strings.Contains(strings.Join(reasons, "; "), tt.reason) {
				t.Fatalf("got %s %v", conf, reasons)
			}
			if tt.want == policy.ConfidenceHigh && len(reasons) > 0 {
				t.Fatal(reasons)
			}
		})
	}
}
