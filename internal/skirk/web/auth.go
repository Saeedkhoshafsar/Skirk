package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookieName = "skirk_session"
	csrfCookieName    = "skirk_csrf"
	csrfHeaderName    = "X-Skirk-CSRF"
	bearerPrefix      = "Bearer "

	// sessionTTL is how long a successful /api/login session is valid for.
	// It is intentionally short because Skirk's web UI is a local admin
	// console: operators can simply re-login if their session expires.
	sessionTTL = 12 * time.Hour

	// loginRateWindow and loginRateMax cap the number of failed /api/login
	// attempts per IP per window to slow down brute-force scans.
	loginRateWindow = time.Minute
	loginRateMax    = 5
)

// AuthConfig holds the long-lived secret used to authenticate Web UI users.
// In PR #1 a single shared bearer token is used (printed on startup and
// persisted to web-token in the kit directory); future PRs may layer per-user
// accounts on top of the same primitives.
type AuthConfig struct {
	// Token is the master bearer token. Empty means auth is disabled
	// (only allowed when binding to a loopback interface).
	Token string

	// NoAuth disables auth entirely. The Server constructor refuses to
	// honour this on non-loopback binds for safety.
	NoAuth bool
}

// authenticator enforces bearer-token + session-cookie auth on the HTTP
// handlers. It is safe for concurrent use.
type authenticator struct {
	cfg AuthConfig

	mu       sync.Mutex
	sessions map[string]sessionEntry // sessionID -> entry
	attempts map[string][]time.Time  // remoteIP -> failed login timestamps
}

type sessionEntry struct {
	csrfToken string
	expiresAt time.Time
}

func newAuthenticator(cfg AuthConfig) *authenticator {
	return &authenticator{
		cfg:      cfg,
		sessions: make(map[string]sessionEntry),
		attempts: make(map[string][]time.Time),
	}
}

// randomToken returns n cryptographically random bytes hex-encoded (2n chars).
func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// LoadOrCreateToken reads an existing token from path or, if absent, generates
// a fresh 32-byte hex token and writes it back with mode 0600. If path is
// empty no file is touched and a fresh token is returned.
func LoadOrCreateToken(path string) (token string, created bool, err error) {
	path = strings.TrimSpace(path)
	if path != "" {
		if data, readErr := os.ReadFile(path); readErr == nil {
			t := strings.TrimSpace(string(data))
			if t != "" {
				return t, false, nil
			}
		} else if !errors.Is(readErr, os.ErrNotExist) {
			return "", false, fmt.Errorf("read token file %s: %w", path, readErr)
		}
	}
	fresh, err := randomToken(32)
	if err != nil {
		return "", false, fmt.Errorf("generate web token: %w", err)
	}
	if path != "" {
		if dir := filepath.Dir(path); dir != "" {
			if mkErr := os.MkdirAll(dir, 0o700); mkErr != nil {
				return "", false, fmt.Errorf("create token dir %s: %w", dir, mkErr)
			}
		}
		if writeErr := os.WriteFile(path, []byte(fresh+"\n"), 0o600); writeErr != nil {
			return "", false, fmt.Errorf("write token file %s: %w", path, writeErr)
		}
	}
	return fresh, true, nil
}

// loginRequest is the JSON body accepted by POST /api/login.
type loginRequest struct {
	Token string `json:"token"`
}

// loginResponse is the JSON body returned on successful login.
type loginResponse struct {
	OK        bool   `json:"ok"`
	CSRFToken string `json:"csrf_token"`
	ExpiresAt string `json:"expires_at"`
}

// handleLogin validates the bearer token and on success issues a session
// cookie + CSRF cookie. It rate-limits failed attempts per remote IP.
func (a *authenticator) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	remote := clientIP(r)
	if a.tooManyAttempts(remote) {
		writeError(w, http.StatusTooManyRequests, "too many login attempts; try again shortly")
		return
	}

	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	provided := strings.TrimSpace(req.Token)
	if provided == "" {
		// Allow Authorization: Bearer <token> too.
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, bearerPrefix) {
			provided = strings.TrimSpace(strings.TrimPrefix(h, bearerPrefix))
		}
	}
	if !a.tokenMatches(provided) {
		a.recordFailedAttempt(remote)
		writeError(w, http.StatusUnauthorized, "invalid token")
		return
	}

	sid, err := randomToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to issue session")
		return
	}
	csrf, err := randomToken(16)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to issue csrf token")
		return
	}
	expires := time.Now().Add(sessionTTL)

	a.mu.Lock()
	a.sessions[sid] = sessionEntry{csrfToken: csrf, expiresAt: expires}
	a.mu.Unlock()

	secure := r.TLS != nil
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sid,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
	// CSRF cookie is intentionally NOT HttpOnly: the frontend reads it and
	// echoes its value in the X-Skirk-CSRF header for the double-submit
	// pattern.
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    csrf,
		Path:     "/",
		Expires:  expires,
		HttpOnly: false,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})

	writeJSON(w, http.StatusOK, loginResponse{
		OK:        true,
		CSRFToken: csrf,
		ExpiresAt: expires.UTC().Format(time.RFC3339),
	})
}

// handleLogout clears the session cookie and revokes the server-side entry.
func (a *authenticator) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
	}
	clear := func(name string) {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			Expires:  time.Unix(0, 0),
			MaxAge:   -1,
			HttpOnly: name == sessionCookieName,
		})
	}
	clear(sessionCookieName)
	clear(csrfCookieName)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// requireAuth wraps a handler with bearer-token / session-cookie + CSRF check.
// When NoAuth is set the wrapper is a no-op (loopback-only as enforced by the
// Server constructor).
func (a *authenticator) requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.cfg.NoAuth {
			h(w, r)
			return
		}
		// Accept either Authorization: Bearer <token> for API clients
		// (curl/scripts) OR a valid session cookie for browser users.
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, bearerPrefix) {
			token := strings.TrimSpace(strings.TrimPrefix(auth, bearerPrefix))
			if a.tokenMatches(token) {
				// Bearer-token clients are exempt from CSRF: they
				// are not subject to cookie-based confused
				// deputy attacks.
				h(w, r)
				return
			}
			writeError(w, http.StatusUnauthorized, "invalid bearer token")
			return
		}

		sid, csrf, ok := a.sessionFromCookies(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if mutating(r.Method) {
			provided := r.Header.Get(csrfHeaderName)
			if subtle.ConstantTimeCompare([]byte(provided), []byte(csrf)) != 1 {
				writeError(w, http.StatusForbidden, "csrf token mismatch")
				return
			}
		}
		_ = sid
		h(w, r)
	}
}

// sessionFromCookies returns the session id and its associated CSRF token if
// the request carries a valid, unexpired session cookie. It also opportunistically
// garbage-collects expired sessions.
func (a *authenticator) sessionFromCookies(r *http.Request) (sid, csrf string, ok bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return "", "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, found := a.sessions[c.Value]
	if !found {
		return "", "", false
	}
	if time.Now().After(entry.expiresAt) {
		delete(a.sessions, c.Value)
		return "", "", false
	}
	return c.Value, entry.csrfToken, true
}

// tokenMatches performs a constant-time comparison against the configured
// master bearer token.
func (a *authenticator) tokenMatches(provided string) bool {
	if a.cfg.Token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(a.cfg.Token)) == 1
}

// tooManyAttempts returns true if the given remote IP has exceeded the failed
// login budget within loginRateWindow.
func (a *authenticator) tooManyAttempts(ip string) bool {
	if ip == "" {
		return false
	}
	now := time.Now()
	cutoff := now.Add(-loginRateWindow)
	a.mu.Lock()
	defer a.mu.Unlock()
	history := a.attempts[ip]
	keep := history[:0]
	for _, t := range history {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	a.attempts[ip] = keep
	return len(keep) >= loginRateMax
}

// recordFailedAttempt appends a failed-login timestamp for ip.
func (a *authenticator) recordFailedAttempt(ip string) {
	if ip == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.attempts[ip] = append(a.attempts[ip], time.Now())
}

// mutating reports whether the HTTP method changes server state and therefore
// requires CSRF protection.
func mutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// clientIP extracts the peer IP without the port. It does NOT trust
// X-Forwarded-For: the web UI is expected to bind directly, not behind an
// untrusted proxy.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
