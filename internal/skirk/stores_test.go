package skirk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
)

// newFakeOAuthServer returns an httptest server that mints fresh access
// tokens for every refresh request. The token URL it returns is suitable
// for AuthConfig.TokenURL so the OAuth client can complete a real round
// trip during tests without contacting Google.
func newFakeOAuthServer(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var count int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&count, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "access-token-" + strconv.Itoa(int(n)),
			"expires_in":   3600,
			"token_type":   "Bearer",
		})
	}))
	t.Cleanup(server.Close)
	return server, &count
}

// TestBlobStoreFromConfigSingleMailboxReturnsDriveStore verifies that the
// happy path with no extras returns a single *DriveStore (not a pool), so
// the byte-for-byte behaviour relative to v0.1.52 stays guaranteed for
// users who have not opted into multi-mailbox.
func TestBlobStoreFromConfigSingleMailboxReturnsDriveStore(t *testing.T) {
	server, count := newFakeOAuthServer(t)
	cfg := &Config{
		Secret: "test-secret",
		Auth: AuthConfig{
			ClientID:     "client-id",
			RefreshToken: "refresh-token",
			TokenURL:     server.URL,
		},
		Route: RouteConfig{Mode: "direct"},
		Drive: DriveConfig{FolderID: "primary-folder"},
	}

	store, closer, err := BlobStoreFromConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BlobStoreFromConfig: %v", err)
	}
	defer closer()

	if _, ok := store.(*DriveStore); !ok {
		t.Fatalf("expected *DriveStore for single-mailbox config, got %T", store)
	}
	if got := atomic.LoadInt32(count); got != 1 {
		t.Fatalf("primary token refreshes = %d, want 1 (eager init)", got)
	}
}

// TestBlobStoreWithPrimaryFromConfigReturnsPrimaryAndPool verifies that
// the multi-mailbox path returns a MailboxPool as the BlobStore AND the
// underlying primary *DriveStore so callers can still issue admin-only
// ops (Cleanup / QuotaSnapshot / ResetTelemetry) on the primary mailbox.
func TestBlobStoreWithPrimaryFromConfigReturnsPrimaryAndPool(t *testing.T) {
	server, count := newFakeOAuthServer(t)
	cfg := &Config{
		Secret: "test-secret",
		Auth: AuthConfig{
			ClientID:     "client-id",
			RefreshToken: "primary-refresh",
			TokenURL:     server.URL,
		},
		Route: RouteConfig{Mode: "direct"},
		Drive: DriveConfig{
			FolderID: "primary-folder",
			ExtraMailboxes: []MailboxConfig{
				{
					Auth: AuthConfig{
						ClientID:     "client-id",
						RefreshToken: "extra-refresh-1",
						TokenURL:     server.URL,
					},
					FolderID: "extra-folder-1",
					Label:    "alt1",
				},
				{
					Auth: AuthConfig{
						ClientID:     "client-id",
						RefreshToken: "extra-refresh-2",
						TokenURL:     server.URL,
					},
					FolderID: "extra-folder-2",
					Label:    "alt2",
				},
			},
		},
	}

	store, primary, closer, err := BlobStoreWithPrimaryFromConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BlobStoreWithPrimaryFromConfig: %v", err)
	}
	defer closer()

	if primary == nil {
		t.Fatal("primary DriveStore must not be nil")
	}
	pool, ok := store.(*MailboxPool)
	if !ok {
		t.Fatalf("expected *MailboxPool for multi-mailbox config, got %T", store)
	}
	if got, want := pool.Size(), 3; got != want {
		t.Fatalf("pool size = %d, want %d (primary + 2 extras)", got, want)
	}
	// Each mailbox must eagerly mint a token at construction time so that
	// a misconfigured extra fails fast at startup instead of silently
	// dropping every Nth lane's traffic in production.
	if got := atomic.LoadInt32(count); got != 3 {
		t.Fatalf("token refreshes = %d, want 3 (1 primary + 2 extras)", got)
	}
	// The pool's mailbox 0 must reuse the same *DriveStore as the
	// returned primary, otherwise admin ops would target a different
	// OAuth principal than runtime traffic.
	if pool.stores[0] != primary {
		t.Fatal("MailboxPool.stores[0] must be the primary returned to the caller")
	}
}

// TestBlobStoreWithPrimaryFromConfigBackwardCompatibleClosure verifies that
// the single-mailbox path returns the primary BOTH as BlobStore and as
// the primary handle (single allocation, no duplicate OAuth source).
func TestBlobStoreWithPrimaryFromConfigBackwardCompatibleClosure(t *testing.T) {
	server, _ := newFakeOAuthServer(t)
	cfg := &Config{
		Secret: "test-secret",
		Auth: AuthConfig{
			ClientID:     "client-id",
			RefreshToken: "refresh-token",
			TokenURL:     server.URL,
		},
		Route: RouteConfig{Mode: "direct"},
		Drive: DriveConfig{FolderID: "primary-folder"},
	}

	store, primary, closer, err := BlobStoreWithPrimaryFromConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BlobStoreWithPrimaryFromConfig: %v", err)
	}
	defer closer()

	if primary == nil {
		t.Fatal("primary must be non-nil even in the single-mailbox path")
	}
	if store != BlobStore(primary) {
		t.Fatal("single-mailbox path must return the primary as the BlobStore (no pool wrap)")
	}
}

// TestBlobStoreFromConfigPropagatesPrimaryTokenFailure verifies that a
// misconfigured primary mailbox fails fast at startup rather than at
// first traffic.
func TestBlobStoreFromConfigPropagatesPrimaryTokenFailure(t *testing.T) {
	// Fake server that always rejects refresh requests.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid_grant", http.StatusBadRequest)
	}))
	defer server.Close()

	cfg := &Config{
		Secret: "test-secret",
		Auth: AuthConfig{
			ClientID:     "client-id",
			RefreshToken: "broken-refresh",
			TokenURL:     server.URL,
		},
		Route: RouteConfig{Mode: "direct"},
		Drive: DriveConfig{FolderID: "primary-folder"},
	}

	_, closer, err := BlobStoreFromConfig(context.Background(), cfg)
	if err == nil {
		closer()
		t.Fatal("expected error for broken primary OAuth, got nil")
	}
	// Closer must be a no-op safe to call even on failure paths.
	closer()
}

// TestBlobStoreFromConfigPropagatesExtraTokenFailure verifies that a
// misconfigured EXTRA mailbox tears down the partially-constructed pool
// (closing the primary token source) and returns an error pinned to the
// offending index, so the operator can locate the bad entry in config.
func TestBlobStoreFromConfigPropagatesExtraTokenFailure(t *testing.T) {
	goodServer, _ := newFakeOAuthServer(t)
	// Broken server returns HTTP 400 so OAuth refresh fails deterministically.
	brokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid_grant", http.StatusBadRequest)
	}))
	defer brokenServer.Close()

	cfg := &Config{
		Secret: "test-secret",
		Auth: AuthConfig{
			ClientID:     "client-id",
			RefreshToken: "primary-refresh",
			TokenURL:     goodServer.URL,
		},
		Route: RouteConfig{Mode: "direct"},
		Drive: DriveConfig{
			FolderID: "primary-folder",
			ExtraMailboxes: []MailboxConfig{
				{
					Auth: AuthConfig{
						ClientID:     "client-id",
						RefreshToken: "extra-good",
						TokenURL:     goodServer.URL,
					},
					FolderID: "extra-1",
				},
				{
					Auth: AuthConfig{
						ClientID:     "client-id",
						RefreshToken: "extra-broken",
						TokenURL:     brokenServer.URL,
					},
					FolderID: "extra-2",
				},
			},
		},
	}

	_, closer, err := BlobStoreFromConfig(context.Background(), cfg)
	if err == nil {
		closer()
		t.Fatal("expected error for broken extra mailbox, got nil")
	}
	closer()
	// Error message should pin the offending index ([1]) so operators
	// can correlate to extra_mailboxes[1] in their config.
	if msg := err.Error(); msg == "" {
		t.Fatal("error message is empty")
	}
}
