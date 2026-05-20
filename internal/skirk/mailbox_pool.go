package skirk

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// MailboxPool stripes Drive traffic across multiple mailboxes (each backed by
// its own DriveStore with independent OAuth credentials and folder).
//
// The pool satisfies BlobStore plus every optional interface that the mux
// type-asserts at runtime (ObjectPutStore, ObjectPutIDStore,
// ObjectIDReserveStore, ObjectIDStore, RangeObjectStore, FreshListStore,
// FreshListStatusStore, FreshListPageStatusStore,
// FreshListContainsPageStatusStore, ChangeFeedStore, and driveQuotaWaitStore)
// so wrapping is transparent to the rest of the codebase: callers continue
// to assert against the same interfaces as if a single DriveStore was in
// use.
//
// Routing rules:
//   - PUT operations with a name pick a mailbox via FNV-1a hash of the
//     object name modulo pool size. This is deterministic: the same name
//     always lands on the same mailbox, and both client and exit ends pick
//     the same mailbox for a given lane because mux object names already
//     encode lane+sequence.
//   - PUT operations with a pre-reserved fileID use the mailbox that
//     reserved that fileID. GenerateObjectIDs records the fileID → mailbox
//     mapping in an internal bounded LRU.
//   - GET / DELETE by fileID look up the mapping first; on miss they fan
//     out across mailboxes in parallel and return the first hit.
//   - LIST operations fan out across mailboxes and merge results, dropping
//     duplicates by fileID.
//
// Each mailbox keeps its own adaptive backoff. A 429 on one mailbox does
// not stall traffic on the others, which is exactly the property that
// makes multi-mailbox striping a quota-multiplier in practice.
type MailboxPool struct {
	stores []*DriveStore

	// idIndex maps a known fileID → store index. It is populated when a
	// fileID is first observed (either via GenerateObjectIDs reservation
	// or via a successful fan-out lookup). Bounded LRU eviction keeps it
	// from growing unbounded for long-running tunnels.
	idIndex *boundedIDIndex

	logger *log.Logger

	// roundRobinCounter is used to seed GenerateObjectIDs splitting so
	// the first mailbox does not always get the first ID in a batch.
	roundRobinCounter uint64
}

// NewMailboxPool constructs a pool with the given primary and extra
// DriveStores. The primary store is mailbox index 0; extras follow in
// declaration order.
//
// Returns an error if no store was provided or if any entry is nil.
func NewMailboxPool(primary *DriveStore, extras ...*DriveStore) (*MailboxPool, error) {
	if primary == nil {
		return nil, errors.New("mailbox pool requires a non-nil primary store")
	}
	stores := make([]*DriveStore, 0, 1+len(extras))
	stores = append(stores, primary)
	for i, e := range extras {
		if e == nil {
			return nil, fmt.Errorf("mailbox pool extra[%d] is nil", i)
		}
		stores = append(stores, e)
	}
	return &MailboxPool{
		stores:  stores,
		idIndex: newBoundedIDIndex(16 * 1024),
	}, nil
}

// SetLogger installs a logger for pool-level diagnostics (mailbox routing
// decisions, fan-out fallbacks). The underlying stores keep their own
// loggers.
func (p *MailboxPool) SetLogger(l *log.Logger) {
	p.logger = l
}

// Size returns the number of mailboxes managed by the pool, including the
// primary.
func (p *MailboxPool) Size() int {
	if p == nil {
		return 0
	}
	return len(p.stores)
}

// storeForName returns the deterministic mailbox for the given object
// name. FNV-1a was chosen over crypto/sha256 because object name volume in
// a busy tunnel can exceed 10k/sec and only collision distribution matters
// for routing.
func (p *MailboxPool) storeForName(name string) (*DriveStore, int) {
	if len(p.stores) == 1 {
		return p.stores[0], 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	idx := int(h.Sum32() % uint32(len(p.stores)))
	return p.stores[idx], idx
}

// storeForID returns the recorded mailbox for a fileID, or (nil, -1) if no
// mapping is known yet.
func (p *MailboxPool) storeForID(fileID string) (*DriveStore, int) {
	if len(p.stores) == 1 {
		return p.stores[0], 0
	}
	idx, ok := p.idIndex.get(fileID)
	if !ok {
		return nil, -1
	}
	if idx < 0 || idx >= len(p.stores) {
		// Defensive: a corrupted index entry should fall back to fan-out.
		return nil, -1
	}
	return p.stores[idx], idx
}

// recordID notes that fileID is owned by the mailbox at index `idx`. Safe
// to call repeatedly; later calls with the same fileID refresh LRU order.
func (p *MailboxPool) recordID(fileID string, idx int) {
	if fileID == "" || idx < 0 || idx >= len(p.stores) {
		return
	}
	p.idIndex.put(fileID, idx)
}

// nextRoundRobin returns a starting index for batch operations that need
// to spread work across mailboxes without any name/ID hint.
func (p *MailboxPool) nextRoundRobin() int {
	if len(p.stores) <= 1 {
		return 0
	}
	v := atomic.AddUint64(&p.roundRobinCounter, 1)
	return int(v % uint64(len(p.stores)))
}

// --- BlobStore ----------------------------------------------------------

func (p *MailboxPool) Put(ctx context.Context, name string, data []byte) error {
	store, _ := p.storeForName(name)
	return store.Put(ctx, name, data)
}

func (p *MailboxPool) Get(ctx context.Context, name string) ([]byte, error) {
	// Get-by-name uses deterministic routing — only one mailbox can have
	// the object because Put was deterministic too.
	store, _ := p.storeForName(name)
	return store.Get(ctx, name)
}

func (p *MailboxPool) Delete(ctx context.Context, name string) error {
	store, _ := p.storeForName(name)
	return store.Delete(ctx, name)
}

func (p *MailboxPool) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	if len(p.stores) == 1 {
		return p.stores[0].List(ctx, prefix)
	}
	results := make([]ObjectInfo, 0, 64)
	seen := make(map[string]struct{}, 64)
	var firstErr error
	for i, s := range p.stores {
		objs, err := s.List(ctx, prefix)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("mailbox[%d] list: %w", i, err)
			}
			continue
		}
		for _, o := range objs {
			if o.ID != "" {
				if _, dup := seen[o.ID]; dup {
					continue
				}
				seen[o.ID] = struct{}{}
				p.recordID(o.ID, i)
			}
			results = append(results, o)
		}
	}
	if len(results) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return results, nil
}

// --- ObjectPutStore / ObjectPutIDStore ----------------------------------

func (p *MailboxPool) PutObject(ctx context.Context, name string, data []byte) (ObjectInfo, error) {
	store, idx := p.storeForName(name)
	info, err := store.PutObject(ctx, name, data)
	if err == nil && info.ID != "" {
		p.recordID(info.ID, idx)
	}
	return info, err
}

func (p *MailboxPool) PutObjectWithID(ctx context.Context, fileID, name string, data []byte) (ObjectInfo, error) {
	// If we reserved this fileID via GenerateObjectIDs, it already maps
	// to a specific mailbox; use that. Otherwise fall back to name-based
	// routing (matches single-mailbox semantics).
	store, idx := p.storeForID(fileID)
	if store == nil {
		store, idx = p.storeForName(name)
	}
	info, err := store.PutObjectWithID(ctx, fileID, name, data)
	if err == nil {
		p.recordID(fileID, idx)
	}
	return info, err
}

// --- ObjectIDReserveStore -----------------------------------------------

func (p *MailboxPool) GenerateObjectIDs(ctx context.Context, count int) ([]string, error) {
	if count <= 0 {
		return nil, nil
	}
	if len(p.stores) == 1 {
		ids, err := p.stores[0].GenerateObjectIDs(ctx, count)
		return ids, err
	}
	// Spread the reservation across mailboxes round-robin so quota and
	// API latency are shared. Each mailbox reserves at most ceil(count/N)
	// IDs to keep request size bounded.
	n := len(p.stores)
	perMailbox := (count + n - 1) / n
	start := p.nextRoundRobin()
	out := make([]string, 0, count)
	var firstErr error
	for i := 0; i < n && len(out) < count; i++ {
		idx := (start + i) % n
		need := perMailbox
		if remaining := count - len(out); remaining < need {
			need = remaining
		}
		ids, err := p.stores[idx].GenerateObjectIDs(ctx, need)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("mailbox[%d] generate ids: %w", idx, err)
			}
			continue
		}
		for _, id := range ids {
			p.recordID(id, idx)
			out = append(out, id)
		}
	}
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	if len(out) < count {
		// Partial success is acceptable for the caller; mux always
		// checks the returned length. Log so the operator can see it.
		if p.logger != nil {
			p.logger.Printf("mailbox pool: GenerateObjectIDs requested=%d got=%d firstErr=%v", count, len(out), firstErr)
		}
	}
	return out, nil
}

// --- ObjectIDStore ------------------------------------------------------

func (p *MailboxPool) GetByID(ctx context.Context, fileID string) ([]byte, error) {
	if store, _ := p.storeForID(fileID); store != nil {
		data, err := store.GetByID(ctx, fileID)
		if err == nil || !isProbablyMailboxMiss(err) {
			return data, err
		}
		// Miss on the recorded mailbox: the mapping is stale, fall through.
	}
	return p.fanOutGetByID(ctx, fileID)
}

func (p *MailboxPool) fanOutGetByID(ctx context.Context, fileID string) ([]byte, error) {
	if len(p.stores) == 1 {
		return p.stores[0].GetByID(ctx, fileID)
	}
	type result struct {
		data []byte
		err  error
		idx  int
	}
	out := make(chan result, len(p.stores))
	fanCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	for i, s := range p.stores {
		wg.Add(1)
		go func(idx int, st *DriveStore) {
			defer wg.Done()
			data, err := st.GetByID(fanCtx, fileID)
			select {
			case out <- result{data: data, err: err, idx: idx}:
			case <-fanCtx.Done():
			}
		}(i, s)
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	var lastErr error
	for r := range out {
		if r.err == nil {
			p.recordID(fileID, r.idx)
			return r.data, nil
		}
		if !isProbablyMailboxMiss(r.err) {
			lastErr = r.err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("object %s not found in any mailbox", fileID)
	}
	return nil, lastErr
}

func (p *MailboxPool) DeleteID(ctx context.Context, fileID string) error {
	if store, _ := p.storeForID(fileID); store != nil {
		err := store.DeleteID(ctx, fileID)
		if err == nil || !isProbablyMailboxMiss(err) {
			return err
		}
	}
	// Stale mapping or never recorded: try every mailbox. Delete is
	// idempotent; a 404 on a mailbox we didn't own is harmless.
	var firstNonMiss error
	for _, s := range p.stores {
		err := s.DeleteID(ctx, fileID)
		if err == nil {
			return nil
		}
		if !isProbablyMailboxMiss(err) && firstNonMiss == nil {
			firstNonMiss = err
		}
	}
	if firstNonMiss != nil {
		return firstNonMiss
	}
	// All mailboxes reported "not found": treat as success (idempotent).
	return nil
}

// --- RangeObjectStore ---------------------------------------------------

func (p *MailboxPool) GetObjectRangeByID(ctx context.Context, fileID string, start, end int64) ([]byte, ObjectRangeInfo, error) {
	if store, _ := p.storeForID(fileID); store != nil {
		data, info, err := store.GetObjectRangeByID(ctx, fileID, start, end)
		if err == nil || !isProbablyMailboxMiss(err) {
			return data, info, err
		}
	}
	// Fall back to scanning mailboxes serially: range reads are typically
	// large and we do not want N parallel large transfers competing.
	var lastErr error
	for i, s := range p.stores {
		data, info, err := s.GetObjectRangeByID(ctx, fileID, start, end)
		if err == nil {
			p.recordID(fileID, i)
			return data, info, nil
		}
		if !isProbablyMailboxMiss(err) {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("object %s not found in any mailbox", fileID)
	}
	return nil, ObjectRangeInfo{}, lastErr
}

// --- FreshListStore et al. ----------------------------------------------

// listFreshResult holds one mailbox's slice of objects (or its error) along
// with whatever paging metadata the caller asked for. We use the same struct
// for plain ListFresh (which only needs Objects) and ListFreshStatus (which
// also folds Pages/Truncated/Incomplete bits), so a single fan-out helper
// can serve both call sites.
type listFreshResult struct {
	idx        int
	objects    []ObjectInfo
	pages      int
	truncated  bool
	incomplete bool
	err        error
}

// fanOutListFresh runs fn against every mailbox in parallel and returns the
// per-mailbox results in stable order (matching p.stores). It is the building
// block behind ListFresh/ListFreshStatus/ListFreshContainsPageStatus so that
// total wall-clock latency tracks the slowest mailbox rather than the sum of
// all mailbox latencies.
//
// Each goroutine receives its mailbox index and the underlying store; the
// returned error is recorded per-mailbox (rather than aborting the fan-out)
// so a single transient failure on one mailbox does not mask fresh data on
// the others.
func (p *MailboxPool) fanOutListFresh(
	ctx context.Context,
	fn func(ctx context.Context, idx int, store *DriveStore) listFreshResult,
) []listFreshResult {
	results := make([]listFreshResult, len(p.stores))
	var wg sync.WaitGroup
	for i, s := range p.stores {
		wg.Add(1)
		go func(idx int, store *DriveStore) {
			defer wg.Done()
			r := fn(ctx, idx, store)
			r.idx = idx
			results[idx] = r
		}(i, s)
	}
	wg.Wait()
	return results
}

func (p *MailboxPool) ListFresh(ctx context.Context, prefix string, since time.Time) ([]ObjectInfo, error) {
	if len(p.stores) == 1 {
		return p.stores[0].ListFresh(ctx, prefix, since)
	}
	parts := p.fanOutListFresh(ctx, func(ctx context.Context, idx int, s *DriveStore) listFreshResult {
		objs, err := s.ListFresh(ctx, prefix, since)
		return listFreshResult{objects: objs, err: err}
	})
	results := make([]ObjectInfo, 0, 64)
	seen := make(map[string]struct{}, 64)
	var firstErr error
	// Iterate in mailbox-index order so duplicate detection and recordID
	// preserve the same "first mailbox that saw this ID wins" semantics as
	// the previous sequential implementation.
	for _, r := range parts {
		if r.err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("mailbox[%d] list fresh: %w", r.idx, r.err)
			}
			continue
		}
		for _, o := range r.objects {
			if o.ID != "" {
				if _, dup := seen[o.ID]; dup {
					continue
				}
				seen[o.ID] = struct{}{}
				p.recordID(o.ID, r.idx)
			}
			results = append(results, o)
		}
	}
	if len(results) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return results, nil
}

func (p *MailboxPool) ListFreshStatus(ctx context.Context, prefix string, since time.Time) (ObjectListInfo, error) {
	if len(p.stores) == 1 {
		return p.stores[0].ListFreshStatus(ctx, prefix, since)
	}
	parts := p.fanOutListFresh(ctx, func(ctx context.Context, idx int, s *DriveStore) listFreshResult {
		page, err := s.ListFreshStatus(ctx, prefix, since)
		return listFreshResult{
			objects:    page.Objects,
			pages:      page.Pages,
			truncated:  page.Truncated,
			incomplete: page.Incomplete,
			err:        err,
		}
	})
	merged := ObjectListInfo{}
	seen := make(map[string]struct{}, 64)
	var firstErr error
	for _, r := range parts {
		if r.err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("mailbox[%d] list fresh status: %w", r.idx, r.err)
			}
			continue
		}
		merged.Pages += r.pages
		merged.Truncated = merged.Truncated || r.truncated
		merged.Incomplete = merged.Incomplete || r.incomplete
		for _, o := range r.objects {
			if o.ID != "" {
				if _, dup := seen[o.ID]; dup {
					continue
				}
				seen[o.ID] = struct{}{}
				p.recordID(o.ID, r.idx)
			}
			merged.Objects = append(merged.Objects, o)
		}
	}
	// NextPageToken is intentionally cleared: a merged page token is not
	// meaningful across heterogeneous mailboxes. Callers that need
	// pagination should use ListFreshPageStatus directly on a single
	// mailbox or accept that multi-mailbox lists return one merged page.
	merged.NextPageToken = ""
	if len(merged.Objects) == 0 && firstErr != nil {
		return ObjectListInfo{}, firstErr
	}
	return merged, nil
}

func (p *MailboxPool) ListFreshPageStatus(ctx context.Context, prefix string, since time.Time, pageToken string) (ObjectListInfo, error) {
	// For a multi-mailbox pool we fold pagination into a single merged
	// page (see ListFreshStatus). A non-empty pageToken means the mux
	// expects more results from a previous call; we just return an empty
	// page so the loop terminates cleanly. Single-mailbox pools pass
	// through unchanged.
	if len(p.stores) == 1 {
		return p.stores[0].ListFreshPageStatus(ctx, prefix, since, pageToken)
	}
	if pageToken != "" {
		return ObjectListInfo{}, nil
	}
	return p.ListFreshStatus(ctx, prefix, since)
}

func (p *MailboxPool) ListFreshContainsPageStatus(ctx context.Context, contains []string, since time.Time, pageToken string, maxPages int) (ObjectListInfo, error) {
	if len(p.stores) == 1 {
		return p.stores[0].ListFreshContainsPageStatus(ctx, contains, since, pageToken, maxPages)
	}
	if pageToken != "" {
		return ObjectListInfo{}, nil
	}
	parts := p.fanOutListFresh(ctx, func(ctx context.Context, idx int, s *DriveStore) listFreshResult {
		page, err := s.ListFreshContainsPageStatus(ctx, contains, since, "", maxPages)
		return listFreshResult{
			objects:    page.Objects,
			pages:      page.Pages,
			truncated:  page.Truncated,
			incomplete: page.Incomplete,
			err:        err,
		}
	})
	merged := ObjectListInfo{}
	seen := make(map[string]struct{}, len(contains))
	var firstErr error
	for _, r := range parts {
		if r.err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("mailbox[%d] list fresh contains: %w", r.idx, r.err)
			}
			continue
		}
		merged.Pages += r.pages
		merged.Truncated = merged.Truncated || r.truncated
		merged.Incomplete = merged.Incomplete || r.incomplete
		for _, o := range r.objects {
			if o.ID != "" {
				if _, dup := seen[o.ID]; dup {
					continue
				}
				seen[o.ID] = struct{}{}
				p.recordID(o.ID, r.idx)
			}
			merged.Objects = append(merged.Objects, o)
		}
	}
	merged.NextPageToken = ""
	if len(merged.Objects) == 0 && firstErr != nil {
		return ObjectListInfo{}, firstErr
	}
	return merged, nil
}

// --- ChangeFeedStore ----------------------------------------------------

func (p *MailboxPool) ChangesStartPageToken(ctx context.Context) (string, error) {
	// The change feed is per-mailbox. When multiple mailboxes are
	// active, the change feed pathway must not be used (mux falls back
	// to FreshList* APIs which we already merge). Return an empty token
	// so the caller treats this as "no change feed available" rather
	// than silently reading from only the primary.
	if len(p.stores) > 1 {
		return "", nil
	}
	return p.stores[0].ChangesStartPageToken(ctx)
}

func (p *MailboxPool) ListChanges(ctx context.Context, pageToken string, includeRemoved bool) (ChangeListInfo, error) {
	if len(p.stores) > 1 {
		return ChangeListInfo{}, nil
	}
	return p.stores[0].ListChanges(ctx, pageToken, includeRemoved)
}

// --- driveQuotaWaitStore ------------------------------------------------

// WaitForDriveQuota waits until *all* mailboxes report quota headroom for
// the given operation. We deliberately wait on each in sequence rather
// than the minimum: if any mailbox is throttled, the next PUT routed to
// it would block anyway, so paying that wait now avoids holding mux
// queue slots open with stuck work.
func (p *MailboxPool) WaitForDriveQuota(ctx context.Context, op string) error {
	for i, s := range p.stores {
		if err := s.WaitForDriveQuota(ctx, op); err != nil {
			return fmt.Errorf("mailbox[%d] quota wait: %w", i, err)
		}
	}
	return nil
}

// --- helpers ------------------------------------------------------------

// isProbablyMailboxMiss reports whether err looks like "object exists, but
// not in this mailbox" — i.e. a 404 / not-found / no-such-file response
// from the Drive API. Any other error (network, auth, quota) is treated
// as a real failure so the caller does not silently swallow it.
//
// The DriveStore wraps Drive HTTP errors in plain text rather than typed
// errors; matching on a handful of well-known substrings is intentional
// and matches the way the rest of the package classifies these errors.
func isProbablyMailboxMiss(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "404"):
		return true
	case strings.Contains(msg, "drive object not found"):
		return true
	case strings.Contains(msg, "no such file"):
		return true
	case strings.Contains(msg, "file not found"):
		return true
	case strings.Contains(msg, "notfound"):
		return true
	}
	return false
}

// --- bounded LRU for fileID → mailbox index -----------------------------

// boundedIDIndex is a small fixed-capacity LRU map. The implementation is
// intentionally simple (single mutex, linked list via map ordering) to
// keep the contention surface small; fileID resolution is on the
// upload/download hot path but the map operations are O(1).
type boundedIDIndex struct {
	mu       sync.Mutex
	capacity int
	data     map[string]int
	// order keeps insertion order so eviction is FIFO. A true LRU would
	// promote on every read; FIFO is cheaper and produces the same
	// behaviour in practice because fileIDs are read once or twice
	// (upload then download) and then dropped.
	order []string
}

func newBoundedIDIndex(capacity int) *boundedIDIndex {
	if capacity <= 0 {
		capacity = 1024
	}
	return &boundedIDIndex{
		capacity: capacity,
		data:     make(map[string]int, capacity),
		order:    make([]string, 0, capacity),
	}
}

func (b *boundedIDIndex) get(key string) (int, bool) {
	b.mu.Lock()
	v, ok := b.data[key]
	b.mu.Unlock()
	return v, ok
}

func (b *boundedIDIndex) put(key string, value int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.data[key]; ok {
		b.data[key] = value
		return
	}
	if len(b.data) >= b.capacity {
		// Evict the oldest entry. order[0] is the FIFO head.
		if len(b.order) > 0 {
			oldest := b.order[0]
			b.order = b.order[1:]
			delete(b.data, oldest)
		}
	}
	b.data[key] = value
	b.order = append(b.order, key)
}
