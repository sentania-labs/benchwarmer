package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Sign-in codes (ADR 0012). An administrator's elevated helper
// (`benchwarmer login`, started from the tray) registers a random code with
// the management token; the dashboard, opened by the non-elevated tray with
// the code in the URL fragment, redeems it once for a session token. Only
// someone who can read the management token can create a code, codes expire
// in a minute and are redeemed only from this PC, and the session token
// works only from this PC and expires. The management token itself never
// leaves the elevated helper.

// SignInCodeTTL is how long a registered code can be redeemed.
const SignInCodeTTL = time.Minute

// SessionTTL is how long a redeemed session token works.
const SessionTTL = 8 * time.Hour

const (
	maxPendingCodes  = 8
	minCodeLen       = 32
	maxCodeLen       = 128
	maxRedeemFailure = 20 // per minute, then redemption pauses
	maxSessions      = 16
)

// SignIn holds pending codes and the session tokens they were exchanged
// for (only digests of either).
type SignIn struct {
	mu       sync.Mutex
	codes    map[[sha256.Size]byte]time.Time
	sessions map[[sha256.Size]byte]time.Time
	failures int
	failFrom time.Time
	now      func() time.Time
}

// NewSignIn returns an empty SignIn.
func NewSignIn(now func() time.Time) *SignIn {
	if now == nil {
		now = time.Now
	}
	return &SignIn{codes: map[[sha256.Size]byte]time.Time{}, sessions: map[[sha256.Size]byte]time.Time{}, now: now}
}

// ValidSession reports whether tok is a live session token.
func (s *SignIn) ValidSession(tok string) bool {
	if s == nil {
		return false
	}
	d := sha256.Sum256([]byte(tok))
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[d]
	if ok && !s.now().Before(exp) {
		delete(s.sessions, d)
		return false
	}
	return ok
}

var errBadCode = errors.New("code must be 32 to 128 characters of letters, digits, '-' or '_'")

func validCode(c string) bool {
	if len(c) < minCodeLen || len(c) > maxCodeLen {
		return false
	}
	for _, r := range c {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// Register accepts a new code and returns when it expires.
func (s *SignIn) Register(code string) (time.Time, error) {
	if !validCode(code) {
		return time.Time{}, errBadCode
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, exp := range s.codes {
		if !now.Before(exp) {
			delete(s.codes, k)
		}
	}
	if len(s.codes) >= maxPendingCodes {
		return time.Time{}, errors.New("too many sign-in codes pending; wait a minute")
	}
	exp := now.Add(SignInCodeTTL)
	s.codes[sha256.Sum256([]byte(code))] = exp
	return exp, nil
}

// Redeem exchanges a code, once, for a new session token.
func (s *SignIn) Redeem(code string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if now.Sub(s.failFrom) > time.Minute {
		s.failures, s.failFrom = 0, now
	}
	if s.failures >= maxRedeemFailure {
		return "", false
	}
	k := sha256.Sum256([]byte(code))
	exp, ok := s.codes[k]
	delete(s.codes, k)
	if !ok || !now.Before(exp) || !validCode(code) {
		s.failures++
		return "", false
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", false
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	for k, e := range s.sessions {
		if !now.Before(e) {
			delete(s.sessions, k)
		}
	}
	if len(s.sessions) >= maxSessions {
		// Drop the oldest: a new sign-in beats a forgotten tab.
		var oldest [sha256.Size]byte
		first := true
		for k, e := range s.sessions {
			if first || e.Before(s.sessions[oldest]) {
				oldest, first = k, false
			}
		}
		delete(s.sessions, oldest)
	}
	s.sessions[sha256.Sum256([]byte(tok))] = now.Add(SessionTTL)
	return tok, true
}

// SignInCode is the body of both sign-in endpoints.
type SignInCode struct {
	Code string `json:"code"`
}

// SignInRegistered answers a registered code.
type SignInRegistered struct {
	ExpiresAt time.Time `json:"expires_at"`
}

// SignInToken answers a redeemed code: a session token for this PC.
type SignInToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (h *handler) registerSignIn(w http.ResponseWriter, r *http.Request) {
	if h.o.SignIn == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "sign-in codes are not enabled")
		return
	}
	// Only the management token itself may mint codes, not a session.
	if h.o.Auth.Identify(r) != PrincipalManagement {
		writeError(w, http.StatusForbidden, CodeForbidden, "registering a sign-in code needs the management token")
		return
	}
	var in SignInCode
	if !decode(w, r, &in, true, false) {
		return
	}
	exp, err := h.o.SignIn.Register(in.Code)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, SignInRegistered{ExpiresAt: exp})
}

func (h *handler) redeemSignIn(w http.ResponseWriter, r *http.Request) {
	if h.o.SignIn == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "sign-in codes are not enabled")
		return
	}
	// A JSON body forces a CORS preflight, which this API never grants, so
	// a web page cannot even submit guesses (and lock redemption out).
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, CodeBadRequest, "Content-Type must be application/json")
		return
	}
	var in SignInCode
	if !decode(w, r, &in, true, false) {
		return
	}
	tok, ok := h.o.SignIn.Redeem(in.Code)
	if !ok {
		writeError(w, http.StatusUnauthorized, CodeInvalidToken, "sign-in code is not valid or has expired; sign in again from the tray")
		return
	}
	writeJSON(w, http.StatusOK, SignInToken{Token: tok, ExpiresAt: h.o.SignIn.now().Add(SessionTTL)})
}
