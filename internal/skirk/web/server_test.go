package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeOps is a deterministic, in-memory MailboxOps for unit tests. It records
// every call so tests can assert side effects without touching the filesystem.
type fakeOps struct {
	mu       sync.Mutex
	list     []MailboxInfo
	addErr   error
	addRes   AddMailboxResult
	addCalls int
	remErr   error
	remRes   RemoveMailboxResult
	remCalls []string
	proErr   error
	proRes   PromoteMailboxResult
	renErr   error
	renCalls []renameCall
	profile  string
}

type renameCall struct{ Old, New string }

func (f *fakeOps) List(string) ([]MailboxInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]MailboxInfo, len(f.list))
	copy(out, f.list)
	return out, nil
}
func (f *fakeOps) Add(_ context.Context, _ string, _ AddMailboxRequest) (AddMailboxResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addCalls++
	return f.addRes, f.addErr
}
func (f *fakeOps) Remove(_ context.Context, _, label string) (RemoveMailboxResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.remCalls = append(f.remCalls, label)
	return f.remRes, f.remErr
}
func (f *fakeOps) Promote(_ context.Context, _, _ string) (PromoteMailboxResult, error) {
	return f.proRes, f.proErr
}
func (f *fakeOps) Rename(_ context.Context, _, oldL, newL string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renCalls = append(f.renCalls, renameCall{Old: oldL, New: newL})
	return f.renErr
}
func (f *fakeOps) ClientProfile(string) (string, error) { return f.profile, nil }

// newTestServer wires a Server backed by an in-memory MailboxOps and returns
// it along with the fake so tests can assert call history.
func newTestServer(t *testing.T, auth AuthConfig, ops *fakeOps) *Server {
	t.Helper()
	if auth.Token == "" && !auth.NoAuth {
		auth.Token = "test-token-123"
	}
	cfg := Config{
		Addr:   "127.0.0.1:0",
		KitDir: t.TempDir(),
		Auth:   auth,
		Ops:    ops,
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func doRequest(t *testing.T, h http.Handler, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		bodyReader = bytes.NewReader(buf)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestNew_RejectsNoAuthOnPublicBind(t *testing.T) {
	_, err := New(Config{
		Addr:   "0.0.0.0:8787",
		KitDir: "/tmp",
		Auth:   AuthConfig{NoAuth: true},
	})
	if err == nil {
		t.Fatal("expected error when --no-auth is used on non-loopback bind")
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNew_AcceptsNoAuthOnLoopback(t *testing.T) {
	_, err := New(Config{
		Addr:   "127.0.0.1:8787",
		KitDir: "/tmp",
		Auth:   AuthConfig{NoAuth: true},
	})
	if err != nil {
		t.Fatalf("expected loopback --no-auth to be allowed, got %v", err)
	}
}

func TestNew_RequiresTokenWhenAuthEnabled(t *testing.T) {
	_, err := New(Config{
		Addr:   "127.0.0.1:8787",
		KitDir: "/tmp",
		Auth:   AuthConfig{}, // no token, no NoAuth
	})
	if err == nil {
		t.Fatal("expected error when no token and no --no-auth")
	}
}

func TestNew_TLSPairRequired(t *testing.T) {
	_, err := New(Config{
		Addr:        "127.0.0.1:8787",
		KitDir:      "/tmp",
		Auth:        AuthConfig{Token: "t"},
		TLSCertFile: "cert.pem",
		// missing TLSKeyFile
	})
	if err == nil {
		t.Fatal("expected error when only one of --tls-cert/--tls-key is set")
	}
}

func TestStatus_RequiresAuth(t *testing.T) {
	s := newTestServer(t, AuthConfig{Token: "tok"}, &fakeOps{})
	w := doRequest(t, s.Handler(), http.MethodGet, "/api/status", nil, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestStatus_BearerTokenWorks(t *testing.T) {
	s := newTestServer(t, AuthConfig{Token: "tok"}, &fakeOps{list: []MailboxInfo{
		{Index: 0, Label: "primary", IsPrimary: true},
		{Index: 1, Label: "spare1"},
	}})
	w := doRequest(t, s.Handler(), http.MethodGet, "/api/status", nil, map[string]string{
		"Authorization": "Bearer tok",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	var info StatusInfo
	if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.MailboxCount != 2 {
		t.Fatalf("expected mailbox_count=2, got %d", info.MailboxCount)
	}
	if !info.AuthRequired {
		t.Fatal("expected auth_required=true")
	}
}

func TestStatus_NoAuthMode(t *testing.T) {
	s := newTestServer(t, AuthConfig{NoAuth: true}, &fakeOps{})
	w := doRequest(t, s.Handler(), http.MethodGet, "/api/status", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 in --no-auth mode, got %d", w.Code)
	}
}

func TestLogin_RejectsBadToken(t *testing.T) {
	s := newTestServer(t, AuthConfig{Token: "good"}, &fakeOps{})
	w := doRequest(t, s.Handler(), http.MethodPost, "/api/login",
		loginRequest{Token: "bad"}, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestLogin_SetsSessionCookie(t *testing.T) {
	s := newTestServer(t, AuthConfig{Token: "good"}, &fakeOps{})
	w := doRequest(t, s.Handler(), http.MethodPost, "/api/login",
		loginRequest{Token: "good"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	var sawSession, sawCSRF bool
	for _, c := range cookies {
		if c.Name == sessionCookieName {
			sawSession = true
			if !c.HttpOnly {
				t.Fatal("session cookie must be HttpOnly")
			}
		}
		if c.Name == csrfCookieName {
			sawCSRF = true
			if c.HttpOnly {
				t.Fatal("csrf cookie must NOT be HttpOnly (frontend reads it)")
			}
		}
	}
	if !sawSession || !sawCSRF {
		t.Fatalf("expected both session and csrf cookies, got %+v", cookies)
	}
}

func TestLogin_RateLimit(t *testing.T) {
	s := newTestServer(t, AuthConfig{Token: "good"}, &fakeOps{})
	for i := 0; i < loginRateMax; i++ {
		w := doRequest(t, s.Handler(), http.MethodPost, "/api/login",
			loginRequest{Token: "bad"}, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: expected 401, got %d", i+1, w.Code)
		}
	}
	w := doRequest(t, s.Handler(), http.MethodPost, "/api/login",
		loginRequest{Token: "bad"}, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after %d failed attempts, got %d", loginRateMax, w.Code)
	}
}

func TestSession_CSRFEnforcedOnMutations(t *testing.T) {
	s := newTestServer(t, AuthConfig{Token: "good"}, &fakeOps{})

	// 1. Log in to obtain cookies.
	loginResp := doRequest(t, s.Handler(), http.MethodPost, "/api/login",
		loginRequest{Token: "good"}, nil)
	if loginResp.Code != http.StatusOK {
		t.Fatalf("login failed: %d", loginResp.Code)
	}
	cookies := loginResp.Result().Cookies()
	cookieHeader := joinCookies(cookies)
	var csrf string
	for _, c := range cookies {
		if c.Name == csrfCookieName {
			csrf = c.Value
		}
	}
	if csrf == "" {
		t.Fatal("no csrf cookie issued")
	}

	// 2. DELETE without CSRF must fail with 403.
	w := doRequest(t, s.Handler(), http.MethodDelete, "/api/mailboxes/spare1", nil,
		map[string]string{"Cookie": cookieHeader})
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 without CSRF, got %d (body: %s)", w.Code, w.Body.String())
	}

	// 3. DELETE with correct CSRF should reach the ops layer.
	w = doRequest(t, s.Handler(), http.MethodDelete, "/api/mailboxes/spare1", nil,
		map[string]string{
			"Cookie":       cookieHeader,
			csrfHeaderName: csrf,
		})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with CSRF, got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestMailboxList(t *testing.T) {
	ops := &fakeOps{list: []MailboxInfo{
		{Index: 0, Label: "primary", IsPrimary: true, OAuthMode: "easy"},
		{Index: 1, Label: "spare1", OAuthMode: "personal"},
	}}
	s := newTestServer(t, AuthConfig{Token: "tok"}, ops)
	w := doRequest(t, s.Handler(), http.MethodGet, "/api/mailboxes", nil,
		map[string]string{"Authorization": "Bearer tok"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	var resp struct {
		Mailboxes []MailboxInfo `json:"mailboxes"`
		Count     int           `json:"count"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count != 2 || len(resp.Mailboxes) != 2 {
		t.Fatalf("expected 2 mailboxes, got %+v", resp)
	}
}

func TestMailboxAdd_ValidationErrors(t *testing.T) {
	ops := &fakeOps{addErr: errors.New("should not be called")}
	s := newTestServer(t, AuthConfig{Token: "tok"}, ops)

	cases := []struct {
		name string
		req  AddMailboxRequest
		want int
	}{
		{"bad oauth mode", AddMailboxRequest{OAuthMode: "weird"}, http.StatusBadRequest},
		{"bad label", AddMailboxRequest{Label: " bad label!", OAuthMode: "easy"}, http.StatusBadRequest},
		{"personal needs client", AddMailboxRequest{OAuthMode: "personal"}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doRequest(t, s.Handler(), http.MethodPost, "/api/mailboxes", tc.req,
				map[string]string{"Authorization": "Bearer tok"})
			if w.Code != tc.want {
				t.Fatalf("expected %d, got %d (body: %s)", tc.want, w.Code, w.Body.String())
			}
		})
	}
	if ops.addCalls != 0 {
		t.Fatalf("ops.Add must not be called for validation errors, got %d calls", ops.addCalls)
	}
}

func TestMailboxAdd_NotImplemented(t *testing.T) {
	ops := &fakeOps{addErr: fmt.Errorf("device-code flow over HTTP is not implemented yet")}
	s := newTestServer(t, AuthConfig{Token: "tok"}, ops)
	w := doRequest(t, s.Handler(), http.MethodPost, "/api/mailboxes",
		AddMailboxRequest{Label: "newlabel", OAuthMode: "easy"},
		map[string]string{"Authorization": "Bearer tok"})
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestMailboxRename_ValidatesNewLabel(t *testing.T) {
	ops := &fakeOps{}
	s := newTestServer(t, AuthConfig{Token: "tok"}, ops)
	w := doRequest(t, s.Handler(), http.MethodPatch, "/api/mailboxes/spare1",
		renameRequest{Label: "bad label!"},
		map[string]string{"Authorization": "Bearer tok"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d (body: %s)", w.Code, w.Body.String())
	}
	if len(ops.renCalls) != 0 {
		t.Fatalf("ops.Rename must not be called on validation error")
	}
}

func TestMailboxRename_Success(t *testing.T) {
	ops := &fakeOps{}
	s := newTestServer(t, AuthConfig{Token: "tok"}, ops)
	w := doRequest(t, s.Handler(), http.MethodPatch, "/api/mailboxes/spare1",
		renameRequest{Label: "spare1-renamed"},
		map[string]string{"Authorization": "Bearer tok"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	if len(ops.renCalls) != 1 || ops.renCalls[0] != (renameCall{Old: "spare1", New: "spare1-renamed"}) {
		t.Fatalf("expected one Rename call, got %+v", ops.renCalls)
	}
}

func TestStaticIndex_Served(t *testing.T) {
	s := newTestServer(t, AuthConfig{NoAuth: true}, &fakeOps{})
	w := doRequest(t, s.Handler(), http.MethodGet, "/", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on /, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("expected text/html content type, got %q", ct)
	}
	if !strings.Contains(w.Body.String(), "Skirk") {
		t.Fatalf("index.html should mention Skirk; got: %s", w.Body.String()[:min(200, len(w.Body.String()))])
	}
}

func TestUnknownMailboxSubResource(t *testing.T) {
	s := newTestServer(t, AuthConfig{Token: "tok"}, &fakeOps{})
	w := doRequest(t, s.Handler(), http.MethodGet, "/api/mailboxes/foo/whatever", nil,
		map[string]string{"Authorization": "Bearer tok"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 on unknown sub-resource, got %d", w.Code)
	}
}

func TestLooksLikeSafeLabel(t *testing.T) {
	good := []string{"a", "label-1", "label_1", "label.1", "Mailbox", "x" + strings.Repeat("y", 63)}
	bad := []string{"", " ", "-leading", ".dot", "has space", "has/slash", "has*star", strings.Repeat("a", 65)}
	for _, v := range good {
		if !LooksLikeSafeLabel(v) {
			t.Errorf("expected %q to be a safe label", v)
		}
	}
	for _, v := range bad {
		if LooksLikeSafeLabel(v) {
			t.Errorf("expected %q to be REJECTED", v)
		}
	}
}

// joinCookies stitches a Cookie request header from a Response Set-Cookie slice.
func joinCookies(cs []*http.Cookie) string {
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}
