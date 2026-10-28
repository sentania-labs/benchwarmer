package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/sentania-labs/benchwarmer/internal/secrets"
)

// Access is the permission an endpoint needs.
type Access int

// Access classes (ADR 0007). The management token satisfies every class;
// the agent token satisfies only endpoints that opt in (status, health,
// mode, agent report).
const (
	// AccessRead: GET endpoints. Loopback without a token when
	// security.loopback_trust is true; otherwise a token.
	AccessRead Access = iota
	// AccessWrite: state-changing endpoints. Always a token, whatever the
	// source address.
	AccessWrite
	// AccessLocal: no token, but only from this PC addressed as localhost
	// or a loopback IP (sign-in code redemption).
	AccessLocal
	// AccessModeWrite: AccessWrite plus one narrow tokenless path for the
	// person at this PC (mode changes). Without a token it needs
	// security.loopback_trust, a trusted loopback request, and the browser
	// request barrier below; the handler then limits which modes qualify.
	AccessModeWrite
)

// Principal is who a request authenticated as.
type Principal int

// Principals.
const (
	PrincipalNone Principal = iota // no token presented
	PrincipalInvalid
	PrincipalManagement
	PrincipalAgent
	PrincipalInference
	// PrincipalSession: a dashboard session from a sign-in code. It acts as
	// the management token, but only from this PC.
	PrincipalSession
)

// String names the principal for audit records. A request with no token that
// was authorized anyway came from the person at this PC.
func (p Principal) String() string {
	switch p {
	case PrincipalManagement:
		return "management"
	case PrincipalAgent:
		return "agent"
	case PrincipalSession:
		return "session"
	case PrincipalNone:
		return "local"
	}
	return "unknown"
}

// Authenticator checks management API credentials. It holds only SHA-256
// digests of the tokens, and compares digests in constant time, so neither
// token contents nor token length leak through timing.
type Authenticator struct {
	management, agent, inference [sha256.Size]byte
	sessions                     *SignIn
}

// UseSessions lets session tokens from s authenticate (loopback only).
func (a *Authenticator) UseSessions(s *SignIn) { a.sessions = s }

// NewAuthenticator builds an Authenticator from loaded tokens. The
// inference token is recognised only so it can be refused explicitly.
func NewAuthenticator(t secrets.Tokens) *Authenticator {
	return &Authenticator{
		management: sha256.Sum256([]byte(t.Management)),
		agent:      sha256.Sum256([]byte(t.Agent)),
		inference:  sha256.Sum256([]byte(t.Inference)),
	}
}

// LoadAuthenticator ensures the token files exist under dataDir and builds
// an Authenticator from them.
func LoadAuthenticator(dataDir string, f secrets.Files) (*Authenticator, error) {
	t, err := secrets.EnsureTokens(dataDir, f)
	if err != nil {
		return nil, err
	}
	return NewAuthenticator(t), nil
}

// Identify returns who the request's bearer token belongs to.
func (a *Authenticator) Identify(r *http.Request) Principal {
	tok, ok := bearer(r)
	if !ok {
		return PrincipalNone
	}
	d := sha256.Sum256([]byte(tok))
	// Evaluate every comparison so the time taken does not reveal which
	// token (if any) matched.
	m := subtle.ConstantTimeCompare(d[:], a.management[:])
	g := subtle.ConstantTimeCompare(d[:], a.agent[:])
	i := subtle.ConstantTimeCompare(d[:], a.inference[:])
	switch {
	case m == 1:
		return PrincipalManagement
	case g == 1:
		return PrincipalAgent
	case i == 1:
		return PrincipalInference
	case a.sessions.ValidSession(tok):
		return PrincipalSession
	}
	return PrincipalInvalid
}

// authError is why a request was refused.
type authError struct {
	status int
	code   string
	msg    string
}

// Authorize applies the access rules. agentOK says the endpoint also
// accepts the agent token (the tray and session agent run as the
// interactive user, who can read that token but not the management token).
// loopbackTrust is the live security.loopback_trust setting.
func (a *Authenticator) Authorize(r *http.Request, need Access, agentOK, loopbackTrust bool) *authError {
	if need == AccessLocal {
		if trustedLoopback(r) {
			return nil
		}
		return &authError{http.StatusForbidden, CodeForbidden, "this endpoint is available only on this PC, addressed as localhost or a loopback IP"}
	}
	p := a.Identify(r)
	switch p {
	case PrincipalManagement:
		return nil
	case PrincipalSession:
		if trustedLoopback(r) {
			return nil
		}
		return &authError{http.StatusUnauthorized, CodeInvalidToken, "a dashboard session works only on this PC; use the management token remotely"}
	case PrincipalAgent:
		if agentOK {
			return nil
		}
		return &authError{http.StatusForbidden, CodeForbidden, "the agent token is not valid for this endpoint"}
	case PrincipalInference:
		return &authError{http.StatusForbidden, CodeForbidden, "the inference token is not valid for the management API"}
	case PrincipalInvalid:
		return &authError{http.StatusUnauthorized, CodeInvalidToken, "bearer token is not valid"}
	}
	// No token.
	if need == AccessRead && loopbackTrust && trustedLoopback(r) {
		return nil
	}
	if need == AccessModeWrite && loopbackTrust && trustedLoopback(r) {
		return localWriteBarrier(r)
	}
	if need != AccessWrite && loopbackTrust && isLoopback(r) {
		return &authError{http.StatusUnauthorized, CodeUnauthorized, "loopback requests without a token must address the service as localhost or a loopback IP"}
	}
	return &authError{http.StatusUnauthorized, CodeUnauthorized, "a bearer token is required"}
}

// localWriteBarrier is the cross-site request defence for the tokenless mode
// change. trustedLoopback alone does not stop a hostile web page: its browser
// can send a request straight to http://localhost:<port>, which passes both the
// peer and the Host check. A real browser always labels such a request, so a
// request is refused when it:
//
//   - carries an Origin that is not exactly this listener's own origin
//     (including "null", from sandboxed frames and file pages);
//   - carries Fetch Metadata that is not a same-origin script fetch;
//   - is not application/json, the one body type a page cannot send without a
//     CORS preflight, which this API never grants (no Access-Control-* header
//     is ever sent, and OPTIONS is not a route).
//
// A request with no Origin and no Fetch Metadata is a native client (the
// tray, curl, a script) and is allowed on the remaining checks. None of this
// authenticates anyone: it assumes the person and every process on this PC
// are trusted and only keeps other web pages out. A local process can forge
// every header here.
func localWriteBarrier(r *http.Request) *authError {
	refuse := func(msg string) *authError { return &authError{http.StatusForbidden, CodeForbidden, msg} }
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return refuse("a tokenless mode change must be sent as application/json")
	}
	origins := r.Header.Values("Origin")
	if len(origins) > 1 {
		return refuse("request refused: more than one Origin header")
	}
	site, mode, dest := r.Header.Get("Sec-Fetch-Site"), r.Header.Get("Sec-Fetch-Mode"), r.Header.Get("Sec-Fetch-Dest")
	if len(origins) == 0 {
		if site != "" || mode != "" || dest != "" {
			// A browser always sends Origin with a PUT.
			return refuse("request refused: a browser request without an Origin")
		}
		return nil
	}
	if !sameOrigin(r, origins[0]) {
		return refuse("request refused: it comes from a different origin than this page")
	}
	if site != "" && site != "same-origin" {
		return refuse("request refused: it was not made by a page served from this listener")
	}
	if mode != "" && mode != "cors" && mode != "same-origin" {
		return refuse("request refused: unexpected request mode")
	}
	if dest != "" && dest != "empty" {
		return refuse("request refused: unexpected request destination")
	}
	return nil
}

// sameOrigin reports whether origin is exactly this listener's origin: the
// scheme the listener speaks and the Host the request was addressed to.
func sameOrigin(r *http.Request, origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Opaque != "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return u.Scheme == scheme && u.Host != "" && strings.EqualFold(u.Host, r.Host)
}

// bearer extracts an "Authorization: Bearer <token>" value.
func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	tok = strings.TrimSpace(tok)
	return tok, tok != ""
}

// isLoopback reports whether the TCP peer is a loopback address. It uses
// r.RemoteAddr only: X-Forwarded-For and similar headers are client-supplied
// and never trusted.
func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// trustedLoopback additionally requires the Host header to name a loopback
// address. A browser tricked by DNS rebinding sends the attacker's host name,
// so this keeps a hostile web page from reading tokenless loopback endpoints.
func trustedLoopback(r *http.Request) bool {
	if !isLoopback(r) {
		return false
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
