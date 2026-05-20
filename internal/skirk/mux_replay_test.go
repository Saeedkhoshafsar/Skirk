package skirk

import (
	"testing"
	"time"
)

// TestCheckAndMarkSeqSeenDetectsReplay verifies that the (clientID, runID,
// lane, seq) replay map admits a tuple exactly once and rejects all
// subsequent appearances inside the TTL window. This is the belt-and-
// suspenders defence layered on top of the name-based markSeen map: a
// Drive operator who renames or copies a sealed envelope cannot bypass it,
// because the tuple is only consulted after OpenEnvelope authenticates the
// payload.
func TestCheckAndMarkSeqSeenDetectsReplay(t *testing.T) {
	m := &driveMux{seqSeen: map[muxSeqKey]time.Time{}}

	if got := m.checkAndMarkSeqSeen("client-A", "run-1", 0, 42); got {
		t.Fatalf("first sighting reported as replay")
	}
	if got := m.checkAndMarkSeqSeen("client-A", "run-1", 0, 42); !got {
		t.Fatalf("second sighting NOT reported as replay")
	}

	// A different lane with the same sequence is legitimate (each lane
	// has its own AES-GCM key and counter) and must be admitted.
	if got := m.checkAndMarkSeqSeen("client-A", "run-1", 1, 42); got {
		t.Fatalf("lane 1 seq 42 spuriously rejected as replay")
	}
	// A different runID with the same lane+seq is also legitimate
	// (sessions rotate runIDs to refresh keys).
	if got := m.checkAndMarkSeqSeen("client-A", "run-2", 0, 42); got {
		t.Fatalf("run-2 lane 0 seq 42 spuriously rejected as replay")
	}
}

// TestCheckAndMarkSeqSeenEvictsByTTL verifies that when the seqSeen map
// grows past muxSeqSeenCap, TTL-based compaction kicks in and frees old
// entries instead of growing without bound. The test temporarily lowers
// both the cap and the TTL so it can drive the eviction path without
// inserting hundreds of thousands of entries.
func TestCheckAndMarkSeqSeenEvictsByTTL(t *testing.T) {
	origCap := muxSeqSeenCap
	origTTL := muxSeqSeenTTL
	muxSeqSeenCap = 4
	muxSeqSeenTTL = 50 * time.Millisecond
	t.Cleanup(func() {
		muxSeqSeenCap = origCap
		muxSeqSeenTTL = origTTL
	})

	m := &driveMux{seqSeen: map[muxSeqKey]time.Time{}}

	// Insert four old entries (timestamps deliberately past the TTL
	// cutoff) so the next insertion will trigger compaction.
	oldTime := time.Now().Add(-time.Hour)
	for seq := uint64(0); seq < 4; seq++ {
		m.seqSeen[muxSeqKey{clientID: "c", runID: "r", lane: 0, seq: seq}] = oldTime
	}
	if got := len(m.seqSeen); got != 4 {
		t.Fatalf("pre-trigger map size = %d, want 4", got)
	}

	// This insertion pushes the map over the cap and must evict the
	// stale entries.
	if got := m.checkAndMarkSeqSeen("c", "r", 0, 99); got {
		t.Fatalf("fresh tuple reported as replay")
	}
	if got := len(m.seqSeen); got > muxSeqSeenCap {
		t.Fatalf("post-trigger map size = %d, want <= %d", got, muxSeqSeenCap)
	}
	// The fresh entry must still be present so subsequent duplicates of
	// it are still caught.
	if got := m.checkAndMarkSeqSeen("c", "r", 0, 99); !got {
		t.Fatalf("fresh entry was evicted along with the stale ones")
	}
}
