package controller

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/events"
	"github.com/sentania-labs/benchwarmer/internal/policy"
)

type attributionEpisode struct {
	pid          int
	since        time.Time
	signature    string
	detail       string
	identityOnly bool
}

// trackAttribution records transitions, not every sampling tick. Missing member
// entries alone are harmless when the retained root still yields high confidence.
func (c *Controller) trackAttribution(now time.Time, in FactInput) {
	rootInMembers := slices.Contains(in.OwnPIDs, in.RuntimePID)
	identityOnly := c.gpu.Confidence == policy.ConfidenceHigh
	degraded := in.RuntimePID > 0 && (!rootInMembers || c.gpu.Confidence != policy.ConfidenceHigh)
	if c.attribution.pid != 0 && (c.attribution.pid != in.RuntimePID || !degraded || c.attribution.identityOnly != identityOnly) {
		old := c.attribution
		status := "recovered"
		if degraded && !identityOnly {
			status = "attribution_changed"
		}
		if old.pid != in.RuntimePID {
			status = "runtime_ended"
		}
		c.emit(events.Event{Time: now, Type: events.TelemetryDegraded, RuntimePID: old.pid,
			Message: fmt.Sprintf("Runtime GPU attribution %s after %s (pid %d); previous: %s", status, now.Sub(old.since), old.pid, old.detail),
			Data:    map[string]any{"status": status, "duration_ms": now.Sub(old.since).Milliseconds(), "since": old.since.Format(time.RFC3339Nano), "identity_only": old.identityOnly}})
		c.attribution = attributionEpisode{}
	}
	if !degraded {
		return
	}
	reasons := slices.Clone(c.gpu.AttributionReasons)
	if !rootInMembers {
		reasons = append(reasons, "runtime root absent from job membership; retaining root identity")
	}
	if c.gpu.Confidence != policy.ConfidenceHigh && len(c.gpu.AttributionReasons) == 0 {
		reasons = append(reasons, "GPU confidence "+string(c.gpu.Confidence))
	}
	seen := map[int]bool{}
	var processBytes, rootBytes uint64
	for _, p := range c.sample.Processes {
		seen[int(p.PID)] = true
		processBytes += p.DedicatedBytes
		if int(p.PID) == in.RuntimePID {
			rootBytes += p.DedicatedBytes
		}
	}
	expected := append(slices.Clone(in.OwnPIDs), in.RuntimePID)
	slices.Sort(expected)
	expected = slices.Compact(expected)
	missing := []int{}
	for _, pid := range expected {
		if !seen[pid] {
			missing = append(missing, pid)
		}
	}
	signature := fmt.Sprintf("%s; missing GPU process PIDs=%v; counter errors=%v", strings.Join(reasons, "; "), missing, c.sample.Errors)
	if c.attribution.pid == 0 {
		c.attribution = attributionEpisode{pid: in.RuntimePID, since: now}
	}
	categories := slices.Clone(reasons)
	for i, reason := range categories {
		categories[i], _, _ = strings.Cut(reason, ": adapter=")
	}
	key := fmt.Sprintf("%v/%v/%v", categories, missing, c.sample.Errors)
	if c.attribution.signature == key {
		return
	}
	c.attribution.signature = key
	c.attribution.detail = signature
	c.attribution.identityOnly = identityOnly
	duration := now.Sub(c.attribution.since)
	c.emit(events.Event{Time: now, Type: events.TelemetryDegraded, Severity: policy.SeverityWarning,
		RuntimePID: in.RuntimePID, Telemetry: events.TelemetryFrom(c.gpu),
		Message: fmt.Sprintf("Runtime GPU attribution incomplete for %s (pid %d): %s", duration, in.RuntimePID, signature),
		Data: map[string]any{"status": "degraded", "duration_ms": duration.Milliseconds(), "since": c.attribution.since.Format(time.RFC3339Nano),
			"identity_only": identityOnly, "reasons": reasons, "missing_gpu_pids": missing, "reported_member_pids": slices.Clone(in.OwnPIDs),
			"root_in_members": rootInMembers, "root_in_sample": seen[in.RuntimePID], "root_dedicated_bytes": rootBytes,
			"sample_process_count": len(c.sample.Processes), "sample_complete": c.sample.Complete, "sample_errors": slices.Clone(c.sample.Errors),
			"adapter_instances_present": c.sample.HasAdapterInstances, "invalid_engine_readings": c.sample.InvalidReadings,
			"adapter_dedicated_bytes": c.sample.DedicatedUsedBytes, "process_dedicated_bytes": processBytes, "footprint_mib": in.FootprintMiB}})
}
