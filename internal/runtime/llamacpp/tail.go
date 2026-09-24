package llamacpp

import (
	"regexp"
	"strings"
	"sync"

	"github.com/sentania-labs/benchwarmer/internal/config"
)

// tail keeps the last max bytes written to it. It is the runtime's stdout
// and stderr sink, so it must never block or grow without bound.
type tail struct {
	mu        sync.Mutex
	buf       []byte
	max       int
	truncated bool
}

func newTail(max int) *tail {
	if max <= 0 {
		max = 16384
	}
	return &tail{max: max, buf: make([]byte, 0, max)}
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(p)
	if len(p) >= t.max {
		p = p[len(p)-t.max:]
		t.buf = t.buf[:0]
		t.truncated = true
	}
	if over := len(t.buf) + len(p) - t.max; over > 0 {
		k := copy(t.buf, t.buf[over:])
		t.buf = t.buf[:k]
		t.truncated = true
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

// String returns the retained text. After truncation the first, partial
// line is dropped, so a secret cut in half at the boundary cannot escape
// the patterns that would have matched it whole.
func (t *tail) String() string {
	t.mu.Lock()
	s := string(t.buf)
	trunc := t.truncated
	t.mu.Unlock()
	if trunc {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		} else {
			s = ""
		}
	}
	return s
}

var (
	// "Bearer <token>" anywhere, e.g. an echoed Authorization header.
	bearerRe = regexp.MustCompile(`(?i)(bearer\s+)[^\s"',;]+`)
	// A secret-looking flag or key followed by a value: "--api-key x",
	// "--hf-token=x", "api_key: x", "\"password\": \"x\"". The name must end
	// in the secret word, so model metadata such as "tokenizer.ggml.model"
	// is left alone, and flags naming a file ("--api-key-file") keep their
	// value, matching config.RedactArgs.
	secretKVRe = regexp.MustCompile(`(?i)(^|[\s"'{,(\[])((?:--?)?(?:[\w.]+[_.-])*(?:key|token|secret|password|passwd|credential)["']?)(\s*[=:]\s*|\s+)(["']?)([^\s"',;]+)`)
)

// redact masks secrets in runtime output. secrets are literal values known
// to be sensitive (from the configured arguments); they are masked wherever
// they appear, in addition to the pattern-based rules.
func redact(s string, secrets []string) string {
	for _, v := range secrets {
		if len(v) >= 4 {
			s = strings.ReplaceAll(s, v, config.Redacted)
		}
	}
	s = bearerRe.ReplaceAllString(s, "${1}"+config.Redacted)
	return secretKVRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := secretKVRe.FindStringSubmatch(m)
		pre, name, sep, quote, val := sub[1], sub[2], sub[3], sub[4], sub[5]
		if val == config.Redacted {
			return m
		}
		// A bare word followed by prose ("BOS token 1") is not a flag; only
		// mask when there is a separator or a dash prefix.
		if strings.TrimSpace(sep) == "" && !strings.HasPrefix(name, "-") {
			return m
		}
		return pre + name + sep + quote + config.Redacted
	})
}

// secretValues returns the literal values config.RedactArgs would mask.
func secretValues(args []string) []string {
	red := config.RedactArgs(args)
	var out []string
	for i, a := range args {
		if red[i] == a {
			continue
		}
		if _, v, ok := strings.Cut(a, "="); ok && strings.HasPrefix(a, "-") {
			out = append(out, v)
		} else {
			out = append(out, a)
		}
	}
	return out
}
