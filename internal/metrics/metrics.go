// Package metrics exposes Benchwarmer's Prometheus metrics (spec section 14)
// from a private registry. Every label has a bounded value set: states,
// conditions, confidences, and outcomes are enumerations, and rule names are
// the policy package's constants (anything else collapses to "other").
package metrics

import (
	"math"
	"net/http"
	"regexp"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/sentania-labs/benchwarmer/internal/policy"
	"github.com/sentania-labs/benchwarmer/internal/state"
)

const ns = "benchwarmer"

// Bounded label values.
var (
	LoadResults     = []string{"ok", "failed", "timeout"}
	RequestOutcomes = []string{"ok", "client_error", "upstream_error", "force_closed", "timeout"}
	DrainOutcomes   = []string{"completed", "grace_expired"}
	ConfigResults   = []string{"applied", "rejected"}
	conditions      = []state.Condition{state.Available, state.Yielding, state.Unavailable}
	confidences     = []policy.Confidence{policy.ConfidenceHigh, policy.ConfidenceDegraded, policy.ConfidenceNone}
)

// Metrics holds every collector. Methods are safe for concurrent use.
type Metrics struct {
	reg *prometheus.Registry

	state       *prometheus.GaugeVec
	condition   *prometheus.GaugeVec
	admitting   prometheus.Gauge
	transitions *prometheus.CounterVec

	loads       *prometheus.CounterVec
	loadSeconds prometheus.Histogram

	activeRequests   prometheus.Gauge
	requestSeconds   *prometheus.HistogramVec
	requestsRejected prometheus.Counter

	drains            *prometheus.CounterVec
	preemptions       *prometheus.CounterVec
	cooldowns         prometheus.Counter
	suppressions      prometheus.Counter
	crashes           prometheus.Counter
	telemetryFailures prometheus.Counter
	orphansKilled     prometheus.Counter
	configChanges     *prometheus.CounterVec

	gpuUtil         *prometheus.GaugeVec
	vram            *prometheus.GaugeVec
	temperature     prometheus.Gauge
	confidence      *prometheus.GaugeVec
	vramRelease     prometheus.Histogram
	nextLoadSeconds prometheus.Gauge
	buildInfo       *prometheus.GaugeVec
}

// New builds the metrics on a fresh private registry.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{reg: reg}

	m.state = prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: ns, Name: "state",
		Help: "Internal lifecycle state (1 for the current state, 0 otherwise)."}, []string{"state"})
	m.condition = prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: ns, Name: "condition",
		Help: "User-visible condition (1 for the current condition, 0 otherwise)."}, []string{"condition"})
	m.admitting = prometheus.NewGauge(prometheus.GaugeOpts{Namespace: ns, Name: "ready",
		Help: "1 when the runtime is loaded and admitting inference requests."})
	m.transitions = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "state_transitions_total",
		Help: "Lifecycle state transitions."}, []string{"from", "to"})

	m.loads = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "model_loads_total",
		Help: "Model load attempts by result."}, []string{"result"})
	m.loadSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{Namespace: ns, Name: "model_load_duration_seconds",
		Help:    "Time from runtime start to ready.",
		Buckets: []float64{5, 10, 20, 30, 45, 60, 90, 120, 180, 300, 600}})

	m.activeRequests = prometheus.NewGauge(prometheus.GaugeOpts{Namespace: ns, Name: "active_requests",
		Help: "Inference requests in flight through the proxy."})
	m.requestSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: ns, Name: "request_duration_seconds",
		Help:    "Proxied inference request duration by outcome.",
		Buckets: []float64{0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600}}, []string{"outcome"})
	m.requestsRejected = prometheus.NewCounter(prometheus.CounterOpts{Namespace: ns, Name: "requests_rejected_total",
		Help: "Inference requests rejected with 503 because the worker was not admitting."})

	m.drains = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "drains_total",
		Help: "Graceful drains by outcome."}, []string{"outcome"})
	m.preemptions = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "preemptions_total",
		Help: "Unloads caused by policy, by winning rule and whether requests were force-closed."}, []string{"rule", "forced"})
	m.cooldowns = prometheus.NewCounter(prometheus.CounterOpts{Namespace: ns, Name: "cooldowns_total",
		Help: "Cooldown periods started."})
	m.suppressions = prometheus.NewCounter(prometheus.CounterOpts{Namespace: ns, Name: "suppressions_total",
		Help: "Anti-thrash suppressions started."})
	m.crashes = prometheus.NewCounter(prometheus.CounterOpts{Namespace: ns, Name: "runtime_crashes_total",
		Help: "Unexpected runtime exits."})
	m.telemetryFailures = prometheus.NewCounter(prometheus.CounterOpts{Namespace: ns, Name: "telemetry_failures_total",
		Help: "Failed GPU telemetry samples."})
	m.orphansKilled = prometheus.NewCounter(prometheus.CounterOpts{Namespace: ns, Name: "orphans_killed_total",
		Help: "Runtime processes from a previous worker terminated during reconciliation."})
	m.configChanges = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "config_changes_total",
		Help: "Configuration updates by result."}, []string{"result"})

	m.gpuUtil = prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: ns, Name: "gpu_utilization_percent",
		Help: "GPU utilization: total, own (the runtime), and external (everything else)."}, []string{"scope"})
	m.vram = prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: ns, Name: "gpu_vram_mib",
		Help: "Dedicated VRAM in MiB by kind: used, free, own, external."}, []string{"kind"})
	m.temperature = prometheus.NewGauge(prometheus.GaugeOpts{Namespace: ns, Name: "gpu_temperature_celsius",
		Help: "GPU temperature; NaN when unknown."})
	m.confidence = prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: ns, Name: "telemetry_confidence",
		Help: "Telemetry confidence (1 for the current level, 0 otherwise)."}, []string{"confidence"})
	m.vramRelease = prometheus.NewHistogram(prometheus.HistogramOpts{Namespace: ns, Name: "vram_release_seconds",
		Help:    "Time from runtime termination until its VRAM was released.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 15, 30}})
	m.nextLoadSeconds = prometheus.NewGauge(prometheus.GaugeOpts{Namespace: ns, Name: "next_load_seconds",
		Help: "Seconds until the next load is allowed; 0 when no timer applies."})
	m.buildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: ns, Name: "build_info",
		Help: "Build information; always 1."}, []string{"version"})

	reg.MustRegister(m.state, m.condition, m.admitting, m.transitions, m.loads, m.loadSeconds,
		m.activeRequests, m.requestSeconds, m.requestsRejected, m.drains, m.preemptions, m.cooldowns, m.suppressions,
		m.crashes, m.telemetryFailures, m.orphansKilled, m.configChanges, m.gpuUtil, m.vram, m.temperature,
		m.confidence, m.vramRelease, m.nextLoadSeconds, m.buildInfo,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	// Pre-create every bounded series so rates work from the first scrape.
	for _, from := range state.All {
		m.state.WithLabelValues(string(from))
		for _, to := range state.All {
			if state.CanTransition(from, to) {
				m.transitions.WithLabelValues(string(from), string(to))
			}
		}
	}
	for _, c := range conditions {
		m.condition.WithLabelValues(string(c))
	}
	for _, c := range confidences {
		m.confidence.WithLabelValues(string(c))
	}
	for _, v := range LoadResults {
		m.loads.WithLabelValues(v)
	}
	for _, v := range RequestOutcomes {
		m.requestSeconds.WithLabelValues(v)
	}
	for _, v := range DrainOutcomes {
		m.drains.WithLabelValues(v)
	}
	for _, v := range ConfigResults {
		m.configChanges.WithLabelValues(v)
	}
	m.temperature.Set(math.NaN())
	return m
}

// Handler serves the exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError})
}

// Registry exposes the registry, for registering extra collectors.
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// oneOf returns v if it is in allowed, else "other", bounding cardinality
// against a caller passing an unexpected value.
func oneOf(v string, allowed []string) string {
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	return "other"
}

var ruleName = regexp.MustCompile(`^[a-z_]+\.[a-z_]+$`)

func ruleLabel(r string) string {
	if len(r) > 64 || !ruleName.MatchString(r) {
		return "other"
	}
	return r
}

func stateLabel(s state.State) string {
	if !s.Valid() {
		return "other"
	}
	return string(s)
}

// SetState records the current state, condition, and readiness.
func (m *Metrics) SetState(s state.State) {
	for _, x := range state.All {
		m.state.WithLabelValues(string(x)).Set(b2f(x == s))
	}
	c := s.Condition()
	for _, x := range conditions {
		m.condition.WithLabelValues(string(x)).Set(b2f(x == c))
	}
	m.admitting.Set(b2f(s.Admitting()))
}

// IncTransition counts a state transition.
func (m *Metrics) IncTransition(from, to state.State) {
	m.transitions.WithLabelValues(stateLabel(from), stateLabel(to)).Inc()
}

// IncLoad counts a load attempt; result is ok, failed, or timeout.
func (m *Metrics) IncLoad(result string) { m.loads.WithLabelValues(oneOf(result, LoadResults)).Inc() }

// ObserveLoadSeconds records a successful load's duration.
func (m *Metrics) ObserveLoadSeconds(s float64) { m.loadSeconds.Observe(s) }

// SetActiveRequests records requests in flight.
func (m *Metrics) SetActiveRequests(n int) { m.activeRequests.Set(float64(n)) }

// ObserveRequestSeconds records a finished request; outcome is one of
// RequestOutcomes.
func (m *Metrics) ObserveRequestSeconds(outcome string, s float64) {
	m.requestSeconds.WithLabelValues(oneOf(outcome, RequestOutcomes)).Observe(s)
}

// IncRequestsRejected counts a 503 from the proxy.
func (m *Metrics) IncRequestsRejected() { m.requestsRejected.Inc() }

// IncDrain counts a drain; outcome is completed or grace_expired.
func (m *Metrics) IncDrain(outcome string) {
	m.drains.WithLabelValues(oneOf(outcome, DrainOutcomes)).Inc()
}

// IncPreemption counts an unload caused by rule; forced means requests were
// cut off.
func (m *Metrics) IncPreemption(rule string, forced bool) {
	f := "false"
	if forced {
		f = "true"
	}
	m.preemptions.WithLabelValues(ruleLabel(rule), f).Inc()
}

// IncCooldown counts a cooldown start.
func (m *Metrics) IncCooldown() { m.cooldowns.Inc() }

// IncSuppression counts an anti-thrash suppression start.
func (m *Metrics) IncSuppression() { m.suppressions.Inc() }

// IncRuntimeCrash counts an unexpected runtime exit.
func (m *Metrics) IncRuntimeCrash() { m.crashes.Inc() }

// IncTelemetryFailure counts a failed telemetry sample.
func (m *Metrics) IncTelemetryFailure() { m.telemetryFailures.Inc() }

// IncOrphansKilled counts orphaned runtime processes terminated.
func (m *Metrics) IncOrphansKilled(n int) { m.orphansKilled.Add(float64(n)) }

// IncConfigChange counts a config update; result is applied or rejected.
func (m *Metrics) IncConfigChange(result string) {
	m.configChanges.WithLabelValues(oneOf(result, ConfigResults)).Inc()
}

// SetGPU records current telemetry.
func (m *Metrics) SetGPU(g policy.GPUFacts) {
	m.gpuUtil.WithLabelValues("total").Set(g.TotalUtilPct)
	m.gpuUtil.WithLabelValues("own").Set(g.OwnUtilPct)
	m.gpuUtil.WithLabelValues("external").Set(g.ExternalUtilPct)
	m.vram.WithLabelValues("used").Set(float64(g.VRAMUsedMiB))
	m.vram.WithLabelValues("free").Set(float64(g.VRAMFreeMiB))
	m.vram.WithLabelValues("own").Set(float64(g.OwnVRAMMiB))
	m.vram.WithLabelValues("external").Set(float64(g.ExternalVRAMMiB))
	if g.TemperatureC != nil {
		m.temperature.Set(*g.TemperatureC)
	} else {
		m.temperature.Set(math.NaN())
	}
	for _, c := range confidences {
		m.confidence.WithLabelValues(string(c)).Set(b2f(c == g.Confidence))
	}
}

// ObserveVRAMReleaseSeconds records how long VRAM took to free after a stop.
func (m *Metrics) ObserveVRAMReleaseSeconds(s float64) { m.vramRelease.Observe(s) }

// SetNextLoadSeconds records the time until a load is allowed (0 if none).
func (m *Metrics) SetNextLoadSeconds(s float64) { m.nextLoadSeconds.Set(max(s, 0)) }

// SetBuildInfo records the build version. Call once at startup.
func (m *Metrics) SetBuildInfo(version string) {
	m.buildInfo.Reset()
	m.buildInfo.WithLabelValues(version).Set(1)
}

// ObserveEventSink exports the event sink's loss counters.
func (m *Metrics) ObserveEventSink(dropped, failed func() uint64) {
	m.reg.MustRegister(
		prometheus.NewCounterFunc(prometheus.CounterOpts{Namespace: ns, Name: "events_dropped_total",
			Help: "Audit events dropped because the write buffer was full."}, func() float64 { return float64(dropped()) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{Namespace: ns, Name: "events_write_failures_total",
			Help: "Audit events lost to database write errors."}, func() float64 { return float64(failed()) }),
	)
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
