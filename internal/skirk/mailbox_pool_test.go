package skirk

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestNewMailboxPoolRejectsNilPrimary(t *testing.T) {
	if _, err := NewMailboxPool(nil); err == nil {
		t.Fatal("expected error for nil primary store, got nil")
	}
}

func TestNewMailboxPoolRejectsNilExtra(t *testing.T) {
	primary := &DriveStore{}
	if _, err := NewMailboxPool(primary, nil); err == nil {
		t.Fatal("expected error for nil extra store, got nil")
	}
}

func TestNewMailboxPoolSizeReflectsExtras(t *testing.T) {
	primary := &DriveStore{}
	extras := []*DriveStore{{}, {}, {}}
	pool, err := NewMailboxPool(primary, extras...)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := pool.Size(), 4; got != want {
		t.Fatalf("Size = %d, want %d", got, want)
	}
}

func TestMailboxPoolStoreForNameIsDeterministic(t *testing.T) {
	primary := &DriveStore{}
	pool, err := NewMailboxPool(primary, &DriveStore{}, &DriveStore{}, &DriveStore{})
	if err != nil {
		t.Fatalf("NewMailboxPool: %v", err)
	}
	// Repeating the same name must always yield the same index, and
	// different names should not all collapse to a single index.
	indexes := make(map[int]int)
	for i := 0; i < 100; i++ {
		name := fmt.Sprintf("lane-%d/seq-0001", i)
		_, idx := pool.storeForName(name)
		indexes[idx]++
	}
	if len(indexes) < 2 {
		t.Fatalf("expected names to spread across at least 2 mailboxes, got %d", len(indexes))
	}
	// Determinism: same name → same index across calls.
	const repeatName = "lane-42/seq-0007"
	_, first := pool.storeForName(repeatName)
	for i := 0; i < 50; i++ {
		_, idx := pool.storeForName(repeatName)
		if idx != first {
			t.Fatalf("storeForName non-deterministic: got %d then %d for %q", first, idx, repeatName)
		}
	}
}

func TestMailboxPoolStoreForNameSingleMailboxAlwaysZero(t *testing.T) {
	pool, err := NewMailboxPool(&DriveStore{})
	if err != nil {
		t.Fatalf("NewMailboxPool: %v", err)
	}
	for i := 0; i < 50; i++ {
		_, idx := pool.storeForName(fmt.Sprintf("x-%d", i))
		if idx != 0 {
			t.Fatalf("single-mailbox pool returned idx %d, want 0", idx)
		}
	}
}

func TestMailboxPoolFNVDistributionIsRoughlyEven(t *testing.T) {
	// Spot-check that FNV-1a does not catastrophically skew. We accept
	// any distribution where no single bucket gets more than 60% of the
	// load over a synthetic name stream — far from uniform but more
	// than enough to confirm the hash is doing real work.
	pool, err := NewMailboxPool(&DriveStore{}, &DriveStore{}, &DriveStore{}, &DriveStore{})
	if err != nil {
		t.Fatalf("NewMailboxPool: %v", err)
	}
	counts := make([]int, pool.Size())
	const N = 4000
	for i := 0; i < N; i++ {
		_, idx := pool.storeForName(fmt.Sprintf("skirk/lane-%04d/seq-%06d", i%128, i))
		counts[idx]++
	}
	for i, c := range counts {
		if c > N*60/100 {
			t.Errorf("bucket %d holds %d/%d (>60%%), distribution too skewed: %v", i, c, N, counts)
		}
		if c == 0 {
			t.Errorf("bucket %d got 0 entries, all names collided: %v", i, counts)
		}
	}
}

func TestMailboxPoolStoreForIDFallsBackToFanOut(t *testing.T) {
	pool, err := NewMailboxPool(&DriveStore{}, &DriveStore{}, &DriveStore{})
	if err != nil {
		t.Fatalf("NewMailboxPool: %v", err)
	}
	// An unknown ID returns (nil, -1) so callers know to fan out.
	store, idx := pool.storeForID("unknown-file-id")
	if store != nil {
		t.Errorf("storeForID(unknown) returned non-nil store")
	}
	if idx != -1 {
		t.Errorf("storeForID(unknown) returned idx=%d, want -1", idx)
	}
	// After recording, lookup must return the recorded index.
	pool.recordID("known-id", 2)
	store, idx = pool.storeForID("known-id")
	if store == nil || idx != 2 {
		t.Errorf("storeForID(known) = (%v, %d), want non-nil,2", store, idx)
	}
}

func TestMailboxPoolRecordIDIgnoresOutOfRange(t *testing.T) {
	pool, err := NewMailboxPool(&DriveStore{}, &DriveStore{})
	if err != nil {
		t.Fatalf("NewMailboxPool: %v", err)
	}
	pool.recordID("", 0)             // empty id silently ignored
	pool.recordID("x", -1)           // negative index silently ignored
	pool.recordID("y", 99)           // out-of-range index silently ignored
	pool.recordID("z", pool.Size())  // size itself is out of range
	for _, k := range []string{"", "x", "y", "z"} {
		if _, ok := pool.idIndex.get(k); ok {
			t.Errorf("recordID accepted invalid input for key %q", k)
		}
	}
}

func TestMailboxPoolNextRoundRobinSpreads(t *testing.T) {
	pool, err := NewMailboxPool(&DriveStore{}, &DriveStore{}, &DriveStore{})
	if err != nil {
		t.Fatalf("NewMailboxPool: %v", err)
	}
	seen := make(map[int]bool)
	for i := 0; i < 30; i++ {
		seen[pool.nextRoundRobin()] = true
	}
	if len(seen) != pool.Size() {
		t.Fatalf("nextRoundRobin visited %d/%d mailboxes after 30 calls: %v", len(seen), pool.Size(), seen)
	}
}

func TestMailboxPoolNextRoundRobinSingleMailbox(t *testing.T) {
	pool, err := NewMailboxPool(&DriveStore{})
	if err != nil {
		t.Fatalf("NewMailboxPool: %v", err)
	}
	for i := 0; i < 5; i++ {
		if got := pool.nextRoundRobin(); got != 0 {
			t.Fatalf("single-mailbox round robin returned %d, want 0", got)
		}
	}
}

func TestIsProbablyMailboxMissClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil err is not a miss", nil, false},
		{"404 substring is a miss", errors.New("drive get failed: status=404 body=not found"), true},
		{"NotFound substring is a miss", errors.New("googleapi: NotFound error"), true},
		{"no such file is a miss", errors.New("no such file or directory"), true},
		{"file not found is a miss", errors.New("file not found in drive folder"), true},
		{"drive object not found is a miss", errors.New("drive object not found id=abc"), true},
		{"plain not found string is a miss", errors.New("response code 404 not found"), true},
		{"timeout is NOT a miss", errors.New("context deadline exceeded waiting for response"), false},
		{"auth failure is NOT a miss", errors.New("401 unauthorized token expired"), false},
		{"quota error is NOT a miss", errors.New("429 too many requests user rate limit"), false},
		{"plain network error is NOT a miss", errors.New("connection reset by peer"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isProbablyMailboxMiss(tc.err); got != tc.want {
				t.Errorf("isProbablyMailboxMiss(%q) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestBoundedIDIndexBasic(t *testing.T) {
	idx := newBoundedIDIndex(4)
	if _, ok := idx.get("missing"); ok {
		t.Fatal("empty index returned a hit for unknown key")
	}
	idx.put("a", 1)
	idx.put("b", 2)
	if v, ok := idx.get("a"); !ok || v != 1 {
		t.Errorf("get(a) = (%d, %v), want (1, true)", v, ok)
	}
	if v, ok := idx.get("b"); !ok || v != 2 {
		t.Errorf("get(b) = (%d, %v), want (2, true)", v, ok)
	}
}

func TestBoundedIDIndexUpdateExistingDoesNotEvict(t *testing.T) {
	idx := newBoundedIDIndex(2)
	idx.put("a", 1)
	idx.put("b", 2)
	idx.put("a", 99) // update existing — must not push b out
	if v, ok := idx.get("a"); !ok || v != 99 {
		t.Errorf("get(a) after update = (%d, %v), want (99, true)", v, ok)
	}
	if v, ok := idx.get("b"); !ok || v != 2 {
		t.Errorf("get(b) after a-update = (%d, %v), want (2, true)", v, ok)
	}
}

func TestBoundedIDIndexFIFOEvictsOldest(t *testing.T) {
	idx := newBoundedIDIndex(3)
	idx.put("a", 1)
	idx.put("b", 2)
	idx.put("c", 3)
	idx.put("d", 4) // forces eviction of "a"
	if _, ok := idx.get("a"); ok {
		t.Error("expected oldest key 'a' to be evicted")
	}
	for _, k := range []string{"b", "c", "d"} {
		if _, ok := idx.get(k); !ok {
			t.Errorf("expected key %q to still be present", k)
		}
	}
}

func TestBoundedIDIndexZeroCapacityFallsBackToDefault(t *testing.T) {
	idx := newBoundedIDIndex(0)
	if idx.capacity <= 0 {
		t.Fatalf("zero-capacity index used capacity=%d, want positive", idx.capacity)
	}
	// And it should function correctly.
	idx.put("a", 7)
	if v, ok := idx.get("a"); !ok || v != 7 {
		t.Errorf("get(a) = (%d, %v), want (7, true)", v, ok)
	}
}

// --- Config validation tests for ExtraMailboxes -------------------------

func TestConfigValidatesExtraMailboxes(t *testing.T) {
	base := func() *Config {
		cfg := &Config{}
		cfg.ApplyDefaults()
		// Use a minimal but valid baseline so we only fail on extras.
		cfg.Secret = "test-shared-secret"
		cfg.Auth.RefreshToken = "primary-refresh"
		cfg.Drive.FolderID = "primary-folder"
		cfg.Tunnel.Profile = "fixed"
		return cfg
	}

	t.Run("empty extras is fine", func(t *testing.T) {
		cfg := base()
		if err := cfg.Validate(); err != nil {
			t.Fatalf("unexpected validate error: %v", err)
		}
	})

	t.Run("extra without auth is rejected", func(t *testing.T) {
		cfg := base()
		cfg.Drive.ExtraMailboxes = []MailboxConfig{
			{FolderID: "extra-folder"}, // no auth tokens at all
		}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected validation error for extra without auth")
		}
		if !strings.Contains(err.Error(), "extra_mailboxes[0]") {
			t.Errorf("error should mention extra_mailboxes[0]: %v", err)
		}
	})

	t.Run("extra with refresh token is accepted", func(t *testing.T) {
		cfg := base()
		cfg.Drive.ExtraMailboxes = []MailboxConfig{
			{Auth: AuthConfig{RefreshToken: "extra-r"}, FolderID: "f1", Label: "alt1"},
			{Auth: AuthConfig{AccessToken: "extra-a"}, FolderID: "f2"},
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("unexpected validate error: %v", err)
		}
	})

	t.Run("extra with token_command is accepted", func(t *testing.T) {
		cfg := base()
		cfg.Drive.ExtraMailboxes = []MailboxConfig{
			{Auth: AuthConfig{TokenCommand: "/usr/local/bin/get-token"}, FolderID: "f1"},
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("unexpected validate error: %v", err)
		}
	})

	t.Run("too many extras is rejected", func(t *testing.T) {
		cfg := base()
		for i := 0; i < 16; i++ {
			cfg.Drive.ExtraMailboxes = append(cfg.Drive.ExtraMailboxes, MailboxConfig{
				Auth: AuthConfig{RefreshToken: fmt.Sprintf("r-%d", i)},
			})
		}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected validation error for >15 extras")
		}
		if !strings.Contains(err.Error(), "supports up to 15 entries") {
			t.Errorf("error should mention the 15-entry cap: %v", err)
		}
	})

	t.Run("unsafe label is rejected", func(t *testing.T) {
		cfg := base()
		cfg.Drive.ExtraMailboxes = []MailboxConfig{
			{Auth: AuthConfig{RefreshToken: "r"}, Label: "bad label with spaces"},
		}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected validation error for unsafe label")
		}
		if !strings.Contains(err.Error(), "label") {
			t.Errorf("error should mention label: %v", err)
		}
	})

	t.Run("15 extras at the limit is accepted", func(t *testing.T) {
		cfg := base()
		for i := 0; i < 15; i++ {
			cfg.Drive.ExtraMailboxes = append(cfg.Drive.ExtraMailboxes, MailboxConfig{
				Auth: AuthConfig{RefreshToken: fmt.Sprintf("r-%d", i)},
			})
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("unexpected validate error at limit: %v", err)
		}
	})
}
