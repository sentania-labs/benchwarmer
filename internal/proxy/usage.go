package proxy

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The runtime reports the model and token counts in its response (the
// OpenAI usage object and llama-server's timings), at the head of a stream and
// in its final chunk or the end of a plain body. The tap keeps only the first
// and last few KB of the response, so a long completion costs no memory and
// response text is never logged: only the matched numbers and model name are.
const (
	usageHeadBytes = 4 << 10
	usageTailBytes = 8 << 10
	maxModelLen    = 128
)

var (
	modelRe      = regexp.MustCompile(`"model":\s*"((?:[^"\\]|\\.)*)"`)
	promptRe     = regexp.MustCompile(`"prompt_tokens":\s*(\d+)`)
	completionRe = regexp.MustCompile(`"completion_tokens":\s*(\d+)`)

	// timings.prompt_n leaves out prompt tokens reused from the cache, so it
	// stands in only when the response carries no usage object.
	promptTimingRe     = regexp.MustCompile(`"prompt_n":\s*(\d+)`)
	completionTimingRe = regexp.MustCompile(`"predicted_n":\s*(\d+)`)
)

type usageKey struct{}

// usage accumulates what one request's log line reports about its response.
// It is touched only by the goroutine serving the request: the reverse proxy
// reads the response body there.
type usage struct {
	start    time.Time
	streamed bool
	firstAt  time.Time
	head     []byte
	tail     []byte
}

func (u *usage) observe(p []byte) {
	if len(p) == 0 {
		return
	}
	if u.firstAt.IsZero() {
		u.firstAt = time.Now()
	}
	if room := usageHeadBytes - len(u.head); room > 0 {
		u.head = append(u.head, p[:min(room, len(p))]...)
	}
	u.tail = append(u.tail, p...)
	if len(u.tail) > 2*usageTailBytes {
		u.tail = append(u.tail[:0], u.tail[len(u.tail)-usageTailBytes:]...)
	}
}

// tap wraps a response body so its bytes pass through observe.
type tap struct {
	io.ReadCloser
	u *usage
}

func (t tap) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	t.u.observe(p[:n])
	return n, err
}

// lastInt returns the last match of the first of res that matches in b.
func lastInt(b []byte, res ...*regexp.Regexp) (int, bool) {
	for _, re := range res {
		all := re.FindAllSubmatch(b, -1)
		if len(all) == 0 {
			continue
		}
		n, err := strconv.Atoi(string(all[len(all)-1][1]))
		return n, err == nil
	}
	return 0, false
}

func (u *usage) tailBytes() []byte {
	if len(u.tail) > usageTailBytes {
		return u.tail[len(u.tail)-usageTailBytes:]
	}
	return u.tail
}

// attrs returns the log fields known so far; unknown values are omitted.
func (u *usage) attrs() []any {
	var a []any
	tail := u.tailBytes()
	m := modelRe.FindSubmatch(u.head)
	if m == nil {
		if all := modelRe.FindAllSubmatch(tail, -1); len(all) > 0 {
			m = all[len(all)-1]
		}
	}
	if m != nil {
		model := string(m[1])
		if len(model) > maxModelLen {
			model = model[:maxModelLen]
		}
		a = append(a, "model", model)
	}
	if n, ok := lastInt(tail, promptRe, promptTimingRe); ok {
		a = append(a, "prompt_tokens", n)
	}
	if n, ok := lastInt(tail, completionRe, completionTimingRe); ok {
		a = append(a, "completion_tokens", n)
	}
	if !u.firstAt.IsZero() {
		a = append(a, "ttft_ms", u.firstAt.Sub(u.start).Milliseconds())
	}
	return a
}

// logRequest writes the one-line account of a finished request. Only
// metadata: never the query, body, or headers.
func logRequest(log *slog.Logger, msg string, r *http.Request, id string, status int, outcome string, d time.Duration, u *usage) {
	var shown any = status
	if status == 0 {
		shown = "-"
	}
	a := []any{"request_id", id, "peer", peerHost(r), "method", r.Method, "path", r.URL.Path,
		"status", shown, "outcome", outcome, "streamed", u.streamed}
	a = append(a, u.attrs()...)
	a = append(a, "duration_ms", d.Milliseconds())
	log.Info(msg, a...)
}

func peerHost(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func streamedResponse(h http.Header) bool {
	return strings.HasPrefix(strings.ToLower(h.Get("Content-Type")), "text/event-stream")
}
