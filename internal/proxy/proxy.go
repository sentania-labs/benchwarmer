package proxy

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/api"
	"github.com/sentania-labs/benchwarmer/internal/state"
)

// Response headers added to every proxied or rejected response.
const (
	HeaderCondition = "X-Benchwarmer-Condition"
	HeaderRequestID = "X-Benchwarmer-Request-Id"
)

// Observer receives request accounting. Implementations must be cheap.
type Observer interface {
	RequestFinished(id string, d time.Duration, outcome string)
	RequestRejected()
}

// Options configures the proxy handler.
type Options struct {
	Gate *Gate
	// MaxDuration bounds each request; exceeding it cuts the connection.
	MaxDuration func() time.Duration
	// Token returns the inference token; empty means none configured.
	Token func() string
	// RequireToken forces the token for loopback callers too. Non-loopback
	// callers always need it when one is configured.
	RequireToken func() bool
	// Condition reports the user-visible condition for response headers.
	Condition func() state.Condition
	Observer  Observer
	Log       *slog.Logger
}

// New returns the inference listener's handler. Only /v1/* is served.
func New(o Options) http.Handler {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			t := pr.In.Context().Value(ticketKey{}).(*Ticket)
			pr.SetURL(t.upstream)
			pr.Out.Host = t.upstream.Host
			// The runtime is private; do not forward the caller's credential.
			pr.Out.Header.Del("Authorization")
		},
		// Stream every chunk as soon as it arrives (SSE).
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if cause := context.Cause(r.Context()); errors.Is(cause, ErrPreempted) || errors.Is(cause, errMaxDuration) {
				// Abort without a clean end so the caller sees the cut.
				panic(http.ErrAbortHandler)
			}
			writeError(w, http.StatusBadGateway, api.Error{Error: "runtime unavailable", Code: "upstream_error"})
		},
		ErrorLog: slog.NewLogLogger(o.Log.Handler(), slog.LevelDebug),
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/") {
			writeError(w, http.StatusNotFound, api.Error{Error: "not found", Code: "not_found"})
			return
		}
		cond := state.Unavailable
		if o.Condition != nil {
			cond = o.Condition()
		}
		w.Header().Set(HeaderCondition, string(cond))
		if !authorized(r, o) {
			writeError(w, http.StatusUnauthorized, api.Error{Error: "inference token required", Code: "unauthorized"})
			return
		}
		t, ctx, closed, ok := o.Gate.admit(r.Context())
		if !ok {
			if o.Observer != nil {
				o.Observer.RequestRejected()
			}
			rejectUnavailable(w, closed)
			return
		}
		start := time.Now()
		outcome := "ok"
		w.Header().Set(HeaderRequestID, t.ID)
		defer func() {
			d := time.Since(start)
			if rec := recover(); rec != nil {
				outcome = "force_closed"
				if errors.Is(context.Cause(ctx), errMaxDuration) {
					outcome = "timeout"
				}
				// Only metadata: never the path's query, body, or headers.
				o.Log.Info("inference request cut", "request_id", t.ID, "outcome", outcome, "duration_ms", d.Milliseconds())
				if o.Observer != nil {
					o.Observer.RequestFinished(t.ID, d, outcome)
				}
				// Release last: it signals the controller that the request
				// is fully accounted for.
				o.Gate.release(t)
				panic(rec)
			}
			o.Log.Debug("inference request", "request_id", t.ID, "method", r.Method, "path", r.URL.Path, "outcome", outcome, "duration_ms", d.Milliseconds())
			if o.Observer != nil {
				o.Observer.RequestFinished(t.ID, d, outcome)
			}
			o.Gate.release(t)
		}()
		if o.MaxDuration != nil {
			if max := o.MaxDuration(); max > 0 {
				timer := time.AfterFunc(max, func() { t.cancel(errMaxDuration) })
				defer timer.Stop()
			}
		}
		sw := &statusWriter{ResponseWriter: w}
		rp.ServeHTTP(sw, r.WithContext(context.WithValue(ctx, ticketKey{}, t)))
		switch {
		case sw.status >= 500:
			outcome = "upstream_error"
		case sw.status >= 400:
			outcome = "client_error"
		}
	})
}

type ticketKey struct{}

var errMaxDuration = errors.New("request exceeded maximum duration")

func authorized(r *http.Request, o Options) bool {
	tok := ""
	if o.Token != nil {
		tok = o.Token()
	}
	need := tok != "" && (!isLoopback(r) || (o.RequireToken != nil && o.RequireToken()))
	if !need {
		return true
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(tok)) == 1
}

func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func rejectUnavailable(w http.ResponseWriter, c Closed) {
	body := api.Error{Error: "inference worker unavailable", Code: "worker_unavailable", Condition: c.Condition, Reason: c.Reason}
	if !c.RetryAfter.IsZero() {
		ra := c.RetryAfter
		body.RetryAfter = &ra
		if secs := int(time.Until(ra).Seconds()); secs > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(secs))
		}
	}
	w.Header().Set(HeaderCondition, string(c.Condition))
	writeError(w, http.StatusServiceUnavailable, body)
}

func writeError(w http.ResponseWriter, status int, e api.Error) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(e)
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// Flush forwards streaming flushes.
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }
