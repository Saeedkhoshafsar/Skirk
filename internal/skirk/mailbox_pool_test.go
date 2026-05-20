package skirk

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
	pool.recordID("", 0)            // empty id silently ignored
	pool.recordID("x", -1)          // negative index silently ignored
	pool.recordID("y", 99)          // out-of-range index silently ignored
	pool.recordID("z", pool.Size()) // size itself is out of range
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

// --- Parallel ListFresh tests -------------------------------------------

// TestParallelListFresh verifies that fanOutListFresh runs the per-mailbox
// callbacks in parallel: when one mailbox is artificially slow, total wall
// clock should be bounded by the slowest call (~max) rather than the sum
// of all calls.
//
// We also verify that:
//   - all results are returned (no goroutines drop their work),
//   - results come back in mailbox-index order (so the downstream
//     duplicate-detection / recordID semantics in ListFresh are preserved).
func TestParallelListFresh(t *testing.T) {
	// Build a pool with 4 mailboxes. The underlying *DriveStore values are
	// never invoked because fanOutListFresh delegates the work to the
	// callback we pass in, so empty stubs are fine.
	pool, err := NewMailboxPool(&DriveStore{}, &DriveStore{}, &DriveStore{}, &DriveStore{})
	if err != nil {
		t.Fatalf("NewMailboxPool: %v", err)
	}

	// Per-mailbox sleeps. mailbox 1 is the "slow" one at 200ms; the others
	// are effectively immediate. Sequential execution would take ~280ms;
	// parallel execution should be ~200ms.
	delays := []time.Duration{
		20 * time.Millisecond,
		200 * time.Millisecond,
		30 * time.Millisecond,
		30 * time.Millisecond,
	}

	var invocations atomic.Int32

	start := time.Now()
	parts := pool.fanOutListFresh(context.Background(), func(ctx context.Context, idx int, _ *DriveStore) listFreshResult {
		invocations.Add(1)
		select {
		case <-time.After(delays[idx]):
		case <-ctx.Done():
			return listFreshResult{err: ctx.Err()}
		}
		return listFreshResult{
			objects: []ObjectInfo{{ID: fmt.Sprintf("id-%d", idx), Name: fmt.Sprintf("name-%d", idx)}},
		}
	})
	elapsed := time.Since(start)

	if got, want := invocations.Load(), int32(4); got != want {
		t.Fatalf("invocations = %d, want %d (one per mailbox)", got, want)
	}

	// Sum of sleeps is 280ms; parallel max is 200ms. Allow generous slack
	// for goroutine scheduling on busy CI runners but still firmly below
	// the sequential sum.
	if elapsed >= 280*time.Millisecond {
		t.Fatalf("fanOutListFresh took %s, expected closer to max(%s)=200ms (sequential sum would be 280ms)", elapsed, delays[1])
	}
	// And it must obviously not have finished before the slowest mailbox.
	if elapsed < 150*time.Millisecond {
		t.Fatalf("fanOutListFresh returned in %s, suspiciously faster than the slowest mailbox (%s)", elapsed, delays[1])
	}

	// Results must come back in mailbox-index order. This is the property
	// that lets ListFresh keep its "first mailbox that saw this ID wins"
	// semantics intact.
	if got, want := len(parts), 4; got != want {
		t.Fatalf("len(parts) = %d, want %d", got, want)
	}
	for i, r := range parts {
		if r.idx != i {
			t.Errorf("parts[%d].idx = %d, want %d", i, r.idx, i)
		}
		if r.err != nil {
			t.Errorf("parts[%d].err = %v, want nil", i, r.err)
		}
		if len(r.objects) != 1 || r.objects[0].ID != fmt.Sprintf("id-%d", i) {
			t.Errorf("parts[%d].objects = %+v, want [id-%d]", i, r.objects, i)
		}
	}
}

// TestParallelListFreshPropagatesPerMailboxError verifies that a failure
// from one mailbox does not abort the fan-out: the other mailboxes still
// return their data, and the error is preserved in its slot.
func TestParallelListFreshPropagatesPerMailboxError(t *testing.T) {
	pool, err := NewMailboxPool(&DriveStore{}, &DriveStore{}, &DriveStore{})
	if err != nil {
		t.Fatalf("NewMailboxPool: %v", err)
	}

	boom := errors.New("mailbox 1 exploded")
	parts := pool.fanOutListFresh(context.Background(), func(ctx context.Context, idx int, _ *DriveStore) listFreshResult {
		if idx == 1 {
			return listFreshResult{err: boom}
		}
		return listFreshResult{
			objects: []ObjectInfo{{ID: fmt.Sprintf("id-%d", idx)}},
		}
	})

	if len(parts) != 3 {
		t.Fatalf("len(parts) = %d, want 3", len(parts))
	}
	if parts[0].err != nil || len(parts[0].objects) != 1 {
		t.Errorf("parts[0] = %+v, want one object and no error", parts[0])
	}
	if !errors.Is(parts[1].err, boom) {
		t.Errorf("parts[1].err = %v, want %v", parts[1].err, boom)
	}
	if parts[2].err != nil || len(parts[2].objects) != 1 {
		t.Errorf("parts[2] = %+v, want one object and no error", parts[2])
	}
}

// TestFanOutListFreshRecordsDuration verifies that every result carries a
// non-zero wall-clock duration (so logSlowMailboxes has something to act
// on) and that durations roughly track the per-mailbox sleeps we asked
// for. We deliberately keep the assertion loose -- this is a timing test
// on a busy CI runner, not a microbenchmark.
func TestFanOutListFreshRecordsDuration(t *testing.T) {
	pool, err := NewMailboxPool(&DriveStore{}, &DriveStore{}, &DriveStore{})
	if err != nil {
		t.Fatalf("NewMailboxPool: %v", err)
	}
	delays := []time.Duration{
		10 * time.Millisecond,
		120 * time.Millisecond,
		10 * time.Millisecond,
	}
	parts := pool.fanOutListFresh(context.Background(), func(ctx context.Context, idx int, _ *DriveStore) listFreshResult {
		select {
		case <-time.After(delays[idx]):
		case <-ctx.Done():
		}
		return listFreshResult{}
	})
	if len(parts) != 3 {
		t.Fatalf("len(parts) = %d, want 3", len(parts))
	}
	for i, r := range parts {
		if r.duration <= 0 {
			t.Errorf("parts[%d].duration = %s, want > 0", i, r.duration)
		}
	}
	// Mailbox 1 is the slow one; allow some slack but it must clearly
	// dominate the fast mailboxes.
	if parts[1].duration < 80*time.Millisecond {
		t.Errorf("slow mailbox duration = %s, want >= 80ms", parts[1].duration)
	}
	if parts[0].duration > 90*time.Millisecond || parts[2].duration > 90*time.Millisecond {
		t.Errorf("fast mailbox durations too high: %s / %s", parts[0].duration, parts[2].duration)
	}
}

// TestLogSlowMailboxesQuietBelowThreshold verifies that a healthy
// fan-out (every mailbox below slowMailboxListThreshold) produces no log
// output -- otherwise the journal would be spammed on every ListFresh in
// steady state.
func TestLogSlowMailboxesQuietBelowThreshold(t *testing.T) {
	pool, err := NewMailboxPool(&DriveStore{}, &DriveStore{}, &DriveStore{})
	if err != nil {
		t.Fatalf("NewMailboxPool: %v", err)
	}
	var sink strings.Builder
	pool.SetLogger(log.New(&sink, "", 0))

	parts := []listFreshResult{
		{idx: 0, duration: 10 * time.Millisecond},
		{idx: 1, duration: 20 * time.Millisecond},
		{idx: 2, duration: 30 * time.Millisecond},
	}
	pool.logSlowMailboxes("list_fresh", parts)
	if sink.Len() != 0 {
		t.Fatalf("logSlowMailboxes logged %q for healthy fan-out, want silence", sink.String())
	}
}

// TestLogSlowMailboxesEmitsOnePerSlowMailbox verifies that each mailbox
// above the threshold produces exactly one log line, and that the line
// names the mailbox index and op label so operators can grep for the
// offending mailbox.
func TestLogSlowMailboxesEmitsOnePerSlowMailbox(t *testing.T) {
	// Temporarily lower the threshold so the test does not have to sleep
	// for 500ms per mailbox. Restored via defer so other tests are not
	// affected by ordering.
	saved := slowMailboxListThreshold
	slowMailboxListThreshold = 50 * time.Millisecond
	defer func() { slowMailboxListThreshold = saved }()

	pool, err := NewMailboxPool(&DriveStore{}, &DriveStore{}, &DriveStore{}, &DriveStore{})
	if err != nil {
		t.Fatalf("NewMailboxPool: %v", err)
	}
	var sink strings.Builder
	pool.SetLogger(log.New(&sink, "", 0))

	parts := []listFreshResult{
		{idx: 0, duration: 10 * time.Millisecond},
		{idx: 1, duration: 200 * time.Millisecond},
		{idx: 2, duration: 5 * time.Millisecond},
		{idx: 3, duration: 80 * time.Millisecond, err: errors.New("boom")},
	}
	pool.logSlowMailboxes("list_fresh_status", parts)
	out := sink.String()
	lines := strings.Count(out, "\n")
	if lines != 2 {
		t.Fatalf("expected 2 slow-mailbox lines, got %d: %q", lines, out)
	}
	if !strings.Contains(out, "mailbox[1]") {
		t.Errorf("missing mailbox[1] in log: %q", out)
	}
	if !strings.Contains(out, "mailbox[3]") {
		t.Errorf("missing mailbox[3] in log: %q", out)
	}
	if !strings.Contains(out, "list_fresh_status") {
		t.Errorf("op label missing in log: %q", out)
	}
	// The error from mailbox[3] should appear so operators can
	// distinguish a slow-but-successful mailbox from a slow-and-failing
	// one without cross-referencing another log stream.
	if !strings.Contains(out, "boom") {
		t.Errorf("expected mailbox error to be included: %q", out)
	}
}

// TestPeekMailboxForIDReportsRecordedIndex verifies that the cheap
// snapshot used by the mux hedge path returns -1 when nothing is known
// and the recorded index otherwise. This is the contract the hedge logic
// in downloadMuxObjectAttempt relies on -- if the snapshot is wrong, the
// hedge would either skip the wrong mailbox or fail to skip the primary
// at all.
func TestPeekMailboxForIDReportsRecordedIndex(t *testing.T) {
	pool, err := NewMailboxPool(&DriveStore{}, &DriveStore{}, &DriveStore{})
	if err != nil {
		t.Fatalf("NewMailboxPool: %v", err)
	}
	if got := pool.PeekMailboxForID("unknown"); got != -1 {
		t.Errorf("PeekMailboxForID(unknown) = %d, want -1", got)
	}
	pool.recordID("file-A", 2)
	if got := pool.PeekMailboxForID("file-A"); got != 2 {
		t.Errorf("PeekMailboxForID(file-A) = %d, want 2", got)
	}
}

// TestPeekMailboxForIDSingleMailboxReturnsMinusOne pins the
// single-mailbox optimisation: there is no useful "other mailbox" to
// hedge against, so the mux must not try to route the hedge anywhere
// special. Returning -1 makes downloadMuxObjectAttempt fall through to
// the plain GetByID path.
func TestPeekMailboxForIDSingleMailboxReturnsMinusOne(t *testing.T) {
	pool, err := NewMailboxPool(&DriveStore{})
	if err != nil {
		t.Fatalf("NewMailboxPool: %v", err)
	}
	pool.recordID("file-A", 0)
	if got := pool.PeekMailboxForID("file-A"); got != -1 {
		t.Errorf("PeekMailboxForID on single-mailbox pool = %d, want -1", got)
	}
}

// TestPeekMailboxForIDNilReceiver guards the defensive nil check. The mux
// stores Data as an interface and may legitimately type-assert against
// mailboxHedgeStore on a value that is nil at the pool layer -- panicking
// there would crash the entire receive loop.
func TestPeekMailboxForIDNilReceiver(t *testing.T) {
	var pool *MailboxPool
	if got := pool.PeekMailboxForID("anything"); got != -1 {
		t.Errorf("nil pool PeekMailboxForID = %d, want -1", got)
	}
}

// TestGetByIDExcludingDegradesOnSingleMailbox verifies that when the pool
// only has one mailbox, the "exclude this mailbox" hint is treated as a
// hint (not a hard requirement) -- otherwise the call would always
// return a fake 404 and break single-mailbox deployments that still go
// through the hedge path.
//
// We cannot exercise the real Drive call without an HTTP client, but we
// can confirm that GetByIDExcluding routes through the same path as
// GetByID on a single-mailbox pool by triggering the fast-path branch
// (exclude index out of range / single mailbox) and asserting the
// goroutine does not deadlock.
func TestGetByIDExcludingDegradesOnSingleMailbox(t *testing.T) {
	pool, err := NewMailboxPool(&DriveStore{})
	if err != nil {
		t.Fatalf("NewMailboxPool: %v", err)
	}
	// We invoke through a goroutine with a tight timeout. The underlying
	// DriveStore HTTP call will fail (nil http client), which is fine --
	// we only care that the routing logic does *not* short-circuit the
	// call and that it returns within the timeout (i.e. does not block
	// on an empty channel because every mailbox was excluded).
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Intentionally ignoring the result: empty DriveStore will panic
		// or error out on the actual HTTP call, but routing must reach
		// it (or fail fast) rather than waiting on the goroutine
		// channel.
		defer func() { recover() }()
		_, _ = pool.GetByIDExcluding(ctx, "any-id", 0)
	}()
	select {
	case <-done:
		// Reached the underlying store (and either errored or panicked
		// inside the recover above). Routing is correct.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("GetByIDExcluding on single-mailbox pool blocked instead of routing to the underlying store")
	}
}

// TestGetByIDExcludingFastPathOnExcludeOutOfRange pins the same
// fast-path on a multi-mailbox pool when excludeMailbox is invalid (the
// caller passed -1 because PeekMailboxForID reported "no mapping yet").
// The function must fall back to plain GetByID rather than launching a
// fan-out with every mailbox excluded -- otherwise the hedge would
// degenerate to "no request issued" and we would lose the fallback path
// described in mailboxHedgeStore's contract.
func TestGetByIDExcludingFastPathOnExcludeOutOfRange(t *testing.T) {
	pool, err := NewMailboxPool(&DriveStore{}, &DriveStore{}, &DriveStore{})
	if err != nil {
		t.Fatalf("NewMailboxPool: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { recover() }()
		// excludeMailbox = -1 must route through plain GetByID.
		_, _ = pool.GetByIDExcluding(ctx, "missing-id", -1)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("GetByIDExcluding with excludeMailbox=-1 blocked instead of falling back to GetByID")
	}
}
