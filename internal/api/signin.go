package api

import (
	"crypto/sha256"
	"errors"
	"net/http"
	"sync"
	"time"
)

// Sign-in codes (ADR 0012). An administrator's elevated helper
// (`benchwarmer login`, started from the tray) registers a random code with
// the management token; the dashboard, opened by the non-elevated tray with
// the code in the URL fragment, redeems it once for the management token.
// Only someone who can read the token file can create a code, codes expire
// in a minute, and they are redeemed only from this PC.

// SignInCodeTTL is how long a registered code can be redeemed.
const SignInCodeTTL = time.Minute

const (
	maxPendingCodes  = 8
	minCodeLen       = 32
	maxCodeLen       = 128
	maxRedeemFailure = 20 // per minute, then redemption pauses
)

// SignIn holds pending codes and the management token they unlock.
type SignIn struct {
	mu       sync.Mutex
	token    string
	codes    map[[sha256.Size]byte]time.Time
	failures int
	failFrom time.Time
	now      func() time.Time
}

// NewSignIn returns a SignIn that hands out token.
func NewSignIn(token string, now func() time.Time) *SignIn {
	if now == nil {
		now = time.Now
	}
	return &SignIn{token: token, codes: map[[sha256.Size]byte]time.Time{}, now: now}
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

// Redeem exchanges a code for the token, once.
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
	return s.token, true
}

// SignInCode is the body of both sign-in endpoints.
type SignInCode struct {
	Code string `json:"code"`
}

// SignInRegistered answers a registered code.
type SignInRegistered struct {
	ExpiresAt time.Time `json:"expires_at"`
}

// SignInToken answers a redeemed code.
type SignInToken struct {
	Token string `json:"token"`
}

func (h *handler) registerSignIn(w http.ResponseWriter, r *http.Request) {
	if h.o.SignIn == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "sign-in codes are not enabled")
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
	var in SignInCode
	if !decode(w, r, &in, true, false) {
		return
	}
	tok, ok := h.o.SignIn.Redeem(in.Code)
	if !ok {
		writeError(w, http.StatusUnauthorized, CodeInvalidToken, "sign-in code is not valid or has expired; sign in again from the tray")
		return
	}
	writeJSON(w, http.StatusOK, SignInToken{Token: tok})
}
