package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"net"
	"net/http"
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
)

// Authenticator checks management API credentials. It holds only SHA-256
// digests of the tokens, and compares digests in constant time, so neither
// token contents nor token length leak through timing.
type Authenticator struct {
	management, agent, inference [sha256.Size]byte
}

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
	if need == AccessRead && loopbackTrust && isLoopback(r) {
		return &authError{http.StatusUnauthorized, CodeUnauthorized, "loopback requests without a token must address the service as localhost or a loopback IP"}
	}
	return &authError{http.StatusUnauthorized, CodeUnauthorized, "a bearer token is required"}
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
