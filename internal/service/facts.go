// Package service assembles Benchwarmer's components into the running
// service: configuration, persistence, telemetry, the controller, the
// inference proxy, the management API, metrics, and the web UI.
package service

import (
	"sync"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/api"
	"github.com/sentania-labs/benchwarmer/internal/controller"
	"github.com/sentania-labs/benchwarmer/internal/observe"
	"github.com/sentania-labs/benchwarmer/internal/policy"
)

// Facts adapts observe.Observer to controller.FactBuilder and holds the
// latest session-agent report, which arrives on API goroutines.
type Facts struct {
	obs *observe.Observer

	mu      sync.Mutex
	agent   *observe.AgentReport
	agentAt time.Time
	stale   func() time.Duration
}

// NewFacts returns a fact builder. staleAfter reports the current
// session-staleness window from config.
func NewFacts(staleAfter func() time.Duration) *Facts {
	return &Facts{obs: observe.New(), stale: staleAfter}
}

var _ controller.FactBuilder = (*Facts)(nil)

// Build implements controller.FactBuilder. The controller calls it under its
// own lock, which satisfies observe.Observer's single-goroutine rule.
func (f *Facts) Build(now time.Time, in controller.FactInput) (policy.GPUFacts, policy.AppFacts, policy.SessionFacts) {
	f.mu.Lock()
	var agent *observe.AgentReport
	if f.agent != nil {
		a := *f.agent
		agent = &a
	}
	at := f.agentAt
	f.mu.Unlock()
	out := f.obs.Observe(now, observe.Input{
		Sample: in.Sample, Processes: in.Procs, OwnPIDs: in.OwnPIDs, RuntimeActive: in.RuntimeBusy,
		FootprintMiB: in.FootprintMiB, Profile: in.Profile, Config: in.Config, Agent: agent, AgentAt: at,
	})
	return out.GPU, out.Apps, out.Session
}

// AgentReport implements controller.FactBuilder.
func (f *Facts) AgentReport(now time.Time, r api.AgentReport) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agent = &observe.AgentReport{SessionID: r.SessionID, ForegroundPID: r.ForegroundPID, ForegroundName: r.ForegroundName,
		ForegroundPath: r.ForegroundPath, Fullscreen: r.Fullscreen, IdleSeconds: r.IdleSeconds, Locked: r.Locked}
	f.agentAt = now
}

// AgentStatus implements controller.FactBuilder.
func (f *Facts) AgentStatus(now time.Time) api.AgentStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.agent == nil {
		return api.AgentStatus{}
	}
	return api.AgentStatus{Connected: now.Sub(f.agentAt) <= f.stale(), LastReport: f.agentAt, SessionID: f.agent.SessionID}
}
