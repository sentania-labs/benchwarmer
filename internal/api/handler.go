package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/config"
	"github.com/sentania-labs/benchwarmer/internal/events"
	"github.com/sentania-labs/benchwarmer/internal/policy"
)

// Body limits.
const (
	maxConfigBody = 1 << 20
	maxSmallBody  = 16 << 10
)

// Events paging limits.
const (
	defaultEventsLimit = 100
	maxEventsLimit     = 1000
	maxEventTypes      = 64
)

// maxReasonLen bounds a drain or reload reason (it lands in an event).
const maxReasonLen = 200

// maxModeDuration bounds a Pause AI duration; AI Priority is bounded by
// modes.max_ai_priority.
const maxModeDuration = 30 * 24 * time.Hour

// Options configures the management handler.
type Options struct {
	Backend Backend
	Auth    *Authenticator
	// Metrics serves GET /metrics; nil answers 404.
	Metrics http.Handler
	Version string
	// SignIn enables sign-in codes (POST /api/v1/signin/*); nil answers 404.
	SignIn *SignIn
	// Setup answers GET /api/v1/setup; nil answers 404.
	Setup func() SetupInfo
	// Logger receives one line per request (method, path, status, peer,
	// duration). Headers, query values, and bodies are never logged. Nil
	// disables request logging.
	Logger *slog.Logger
}

type route struct {
	access Access
	// agent: the agent token is also accepted (tray and session agent).
	agent bool
	limit int64
	fn    func(w http.ResponseWriter, r *http.Request)
}

type handler struct {
	o      Options
	routes map[string]map[string]route // path -> method -> route
}

// New returns the management listener's handler: /api/v1/* and /metrics.
func New(o Options) http.Handler {
	h := &handler{o: o}
	h.routes = map[string]map[string]route{
		"/api/v1/health": {"GET": {AccessRead, true, 0, h.health}},
		"/api/v1/status": {"GET": {AccessRead, true, 0, h.status}},
		"/api/v1/config": {
			"GET": {AccessRead, false, 0, h.getConfig},
			"PUT": {AccessWrite, false, maxConfigBody, h.putConfig},
		},
		"/api/v1/events": {"GET": {AccessRead, false, 0, h.events}},
		"/api/v1/applications": {
			"GET": {AccessRead, false, 0, h.getApplications},
			"PUT": {AccessWrite, false, maxConfigBody, h.putApplications},
		},
		"/api/v1/mode":          {"PUT": {AccessWrite, true, maxSmallBody, h.putMode}},
		"/api/v1/drain":         {"POST": {AccessWrite, false, maxSmallBody, h.drain}},
		"/api/v1/reload":        {"POST": {AccessWrite, false, maxSmallBody, h.reload}},
		"/api/v1/agent/report":  {"POST": {AccessWrite, true, maxSmallBody, h.agentReport}},
		"/api/v1/setup":         {"GET": {AccessRead, false, 0, h.setup}},
		"/api/v1/signin/codes":  {"POST": {AccessWrite, false, maxSmallBody, h.registerSignIn}},
		"/api/v1/signin/redeem": {"POST": {AccessLocal, false, maxSmallBody, h.redeemSignIn}},
		"/metrics":              {"GET": {AccessRead, false, 0, h.metrics}},
	}
	return h
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	h.serve(rec, r)
	if h.o.Logger != nil {
		h.o.Logger.Info("management request", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"peer", peerHost(r), "duration_ms", time.Since(start).Milliseconds())
	}
}

func (h *handler) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	methods, ok := h.routes[r.URL.Path]
	if !ok {
		writeError(w, http.StatusNotFound, CodeNotFound, "no such endpoint")
		return
	}
	rt, ok := methods[r.Method]
	if !ok && r.Method == http.MethodHead {
		rt, ok = methods[http.MethodGet]
	}
	if !ok {
		allow := make([]string, 0, len(methods))
		for m := range methods {
			allow = append(allow, m)
		}
		sort.Strings(allow)
		w.Header().Set("Allow", strings.Join(allow, ", "))
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	cfg, _ := h.o.Backend.Config()
	if ae := h.o.Auth.Authorize(r, rt.access, rt.agent, cfg.Security.LoopbackTrust); ae != nil {
		if ae.status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `Bearer realm="benchwarmer"`)
		}
		writeError(w, ae.status, ae.code, ae.msg)
		return
	}
	if rt.limit > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, rt.limit)
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, maxSmallBody)
	}
	rt.fn(w, r)
}

// decode reads one JSON document into v. strict rejects unknown fields;
// allowEmpty accepts an empty body (leaving v zero). It writes the error
// response itself and returns false on failure.
func decode(w http.ResponseWriter, r *http.Request, v any, strict, allowEmpty bool) bool {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, CodeBodyTooLarge, fmt.Sprintf("request body exceeds %d bytes", mbe.Limit))
			return false
		}
		writeError(w, http.StatusBadRequest, CodeBadRequest, "could not read request body")
		return false
	}
	if len(bytes.TrimSpace(b)) == 0 {
		if allowEmpty {
			return true
		}
		writeError(w, http.StatusBadRequest, CodeBadRequest, "request body is required")
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(v); err != nil {
		// json errors describe the position and field name, never values
		// beyond the offending token type, so they are safe to return.
		writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid JSON: "+jsonErrText(err))
		return false
	}
	if dec.More() {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid JSON: trailing data after document")
		return false
	}
	return true
}

// jsonErrText describes a decode error without quoting input values.
func jsonErrText(err error) string {
	var se *json.SyntaxError
	var te *json.UnmarshalTypeError
	switch {
	case errors.As(err, &se):
		return fmt.Sprintf("syntax error at offset %d", se.Offset)
	case errors.As(err, &te):
		return fmt.Sprintf("field %q must be %s", te.Field, te.Type)
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		return strings.TrimPrefix(err.Error(), "json: ")
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected end of input"
	}
	// Custom unmarshalers (durations) return descriptive, value-free text.
	return err.Error()
}

// backendError maps a Backend failure onto a response. Unrecognised errors
// are logged and reported generically, since their text is internal.
func (h *handler) backendError(w http.ResponseWriter, r *http.Request, err error) {
	var re *RequestError
	var ve *config.ValidationError
	switch {
	case errors.As(err, &ve):
		writeJSON(w, http.StatusBadRequest, Error{Error: "configuration is invalid", Code: CodeInvalidConfig, Details: ve.Errors})
	case errors.As(err, &re):
		writeError(w, re.Status, re.Code, re.Message)
	default:
		if h.o.Logger != nil {
			h.o.Logger.Error("management request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		}
		writeError(w, http.StatusInternalServerError, CodeInternal, "internal error; see the service log")
	}
}

// actor names the caller for audit records: the channel and peer address,
// never a credential.
func actor(r *http.Request) string { return "api@" + peerHost(r) }

func peerHost(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func (h *handler) health(w http.ResponseWriter, r *http.Request) {
	s := h.o.Backend.Status()
	out := Health{OK: true, Version: h.o.Version}
	if s.ConfigSource != "primary" {
		out.Problems = append(out.Problems, fmt.Sprintf("configuration loaded from %s copy", s.ConfigSource))
	}
	if s.GPU.Confidence == policy.ConfidenceNone {
		out.Problems = append(out.Problems, "GPU telemetry unavailable")
	}
	out.OK = len(out.Problems) == 0
	writeJSON(w, http.StatusOK, out)
}

func (h *handler) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.o.Backend.Status())
}

func (h *handler) getConfig(w http.ResponseWriter, r *http.Request) {
	c, src := h.o.Backend.Config()
	writeJSON(w, http.StatusOK, ConfigResponse{Config: config.Redact(c), Source: src})
}

func (h *handler) putConfig(w http.ResponseWriter, r *http.Request) {
	var in config.Config
	if !decode(w, r, &in, true, false) {
		return
	}
	cur, _ := h.o.Backend.Config()
	h.applyConfig(w, r, config.RestoreRedacted(in, cur))
}

func (h *handler) applyConfig(w http.ResponseWriter, r *http.Request, c config.Config) {
	// The Backend validates too; checking here guarantees the 400 shape
	// whatever the Backend returns.
	if err := config.Validate(c); err != nil {
		h.backendError(w, r, err)
		return
	}
	im, err := h.o.Backend.UpdateConfig(r.Context(), c, actor(r))
	if err != nil {
		h.backendError(w, r, err)
		return
	}
	nc, src := h.o.Backend.Config()
	writeJSON(w, http.StatusOK, ConfigResponse{Config: config.Redact(nc), Source: src, Impact: &im})
}

func (h *handler) getApplications(w http.ResponseWriter, r *http.Request) {
	c, _ := h.o.Backend.Config()
	apps := c.Applications
	if apps == nil {
		apps = []config.AppRule{}
	}
	writeJSON(w, http.StatusOK, ApplicationsBody{Applications: apps})
}

func (h *handler) putApplications(w http.ResponseWriter, r *http.Request) {
	var in ApplicationsBody
	if !decode(w, r, &in, true, false) {
		return
	}
	if in.Applications == nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, `"applications" is required (use [] for none)`)
		return
	}
	c, _ := h.o.Backend.Config()
	c = config.Clone(c)
	c.Applications = in.Applications
	if err := config.Validate(c); err != nil {
		h.backendError(w, r, err)
		return
	}
	if _, err := h.o.Backend.UpdateConfig(r.Context(), c, actor(r)); err != nil {
		h.backendError(w, r, err)
		return
	}
	nc, _ := h.o.Backend.Config()
	writeJSON(w, http.StatusOK, ApplicationsBody{Applications: nc.Applications})
}

func (h *handler) events(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	eq := EventQuery{Limit: defaultEventsLimit}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, CodeBadRequest, "limit must be a positive integer")
			return
		}
		eq.Limit = min(n, maxEventsLimit)
	}
	if v := q.Get("before_id"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, CodeBadRequest, "before_id must be a positive integer")
			return
		}
		eq.BeforeID = n
	}
	for _, v := range q["type"] {
		for t := range strings.SplitSeq(v, ",") {
			if t = strings.TrimSpace(t); t != "" {
				eq.Types = append(eq.Types, events.Type(t))
			}
		}
	}
	if len(eq.Types) > maxEventTypes {
		writeError(w, http.StatusBadRequest, CodeBadRequest, fmt.Sprintf("at most %d type filters", maxEventTypes))
		return
	}
	// Ask for one extra to learn whether an older page exists.
	want := eq.Limit
	eq.Limit++
	evs, err := h.o.Backend.Events(eq)
	if err != nil {
		h.backendError(w, r, err)
		return
	}
	out := EventsResponse{Events: evs}
	if len(evs) > want {
		out.Events = evs[:want]
		out.NextBeforeID = out.Events[want-1].ID
	}
	if out.Events == nil {
		out.Events = []events.Event{}
	}
	writeJSON(w, http.StatusOK, out)
}

// validDuration reports whether d is an accepted ModeRequest.Duration for
// mode, returning a client-facing reason when not.
func validDuration(mode policy.Mode, d string, maxAIPriority time.Duration) (string, bool) {
	switch d {
	case "":
		return "", true
	case "until_reboot", "until_tomorrow":
		if mode == policy.ModeAuto {
			return "auto takes no duration", false
		}
		return "", true
	}
	if mode == policy.ModeAuto {
		return "auto takes no duration", false
	}
	v, err := time.ParseDuration(d)
	if err != nil {
		return `duration must be a Go duration such as "30m" or "2h", "until_reboot", or "until_tomorrow"`, false
	}
	if v <= 0 {
		return "duration must be positive", false
	}
	limit := maxModeDuration
	if mode == policy.ModeAIPriority && maxAIPriority > 0 {
		limit = maxAIPriority
	}
	if v > limit {
		return fmt.Sprintf("duration must be at most %s", limit), false
	}
	return "", true
}

func (h *handler) putMode(w http.ResponseWriter, r *http.Request) {
	var in ModeRequest
	if !decode(w, r, &in, true, false) {
		return
	}
	switch in.Mode {
	case policy.ModeAuto, policy.ModePause, policy.ModeAIPriority:
	default:
		writeError(w, http.StatusBadRequest, CodeInvalidMode, `mode must be "auto", "pause", or "ai_priority"`)
		return
	}
	cfg, _ := h.o.Backend.Config()
	if msg, ok := validDuration(in.Mode, in.Duration, cfg.Modes.MaxAIPriority.D()); !ok {
		writeError(w, http.StatusBadRequest, CodeInvalidMode, msg)
		return
	}
	in.SetBy = strings.TrimSpace(in.SetBy)
	if in.SetBy == "" {
		in.SetBy = "api"
	}
	if len(in.SetBy) > 64 {
		writeError(w, http.StatusBadRequest, CodeInvalidMode, "set_by must be at most 64 characters")
		return
	}
	ms, err := h.o.Backend.SetMode(in)
	if err != nil {
		h.backendError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ms)
}

func (h *handler) action(w http.ResponseWriter, r *http.Request, name string, fn func(string) error) {
	var in ActionRequest
	if !decode(w, r, &in, true, true) {
		return
	}
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		reason = "requested via API"
	}
	if len(reason) > maxReasonLen {
		writeError(w, http.StatusBadRequest, CodeBadRequest, fmt.Sprintf("reason must be at most %d characters", maxReasonLen))
		return
	}
	if err := fn(reason); err != nil {
		h.backendError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, ActionResponse{Accepted: true, Action: name, Reason: reason})
}

func (h *handler) drain(w http.ResponseWriter, r *http.Request) {
	h.action(w, r, "drain", h.o.Backend.RequestDrain)
}

func (h *handler) reload(w http.ResponseWriter, r *http.Request) {
	h.action(w, r, "reload", h.o.Backend.RequestReload)
}

func (h *handler) agentReport(w http.ResponseWriter, r *http.Request) {
	var in AgentReport
	// Not strict: a newer agent may send fields this service ignores.
	if !decode(w, r, &in, false, false) {
		return
	}
	h.o.Backend.AgentReport(in)
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) metrics(w http.ResponseWriter, r *http.Request) {
	cfg, _ := h.o.Backend.Config()
	if h.o.Metrics == nil || !cfg.Metrics.Enabled {
		writeError(w, http.StatusNotFound, CodeNotFound, "metrics are disabled")
		return
	}
	h.o.Metrics.ServeHTTP(w, r)
}

// statusRecorder captures the response status for the request log.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status, s.wroteHeader = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wroteHeader = true
	return s.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
