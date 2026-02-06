package storage

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	rlqspb "github.com/envoyproxy/go-control-plane/envoy/service/rate_limit_quota/v3"
)

// CanonicalBucketKey is the string representation of a BucketId with sorted keys,
// ensuring order-independent matching.
type CanonicalBucketKey string

// CanonicalizeBucketId produces a deterministic string key from a BucketId by
// sorting the map keys lexicographically.
func CanonicalizeBucketId(id *rlqspb.BucketId) CanonicalBucketKey {
	if id == nil || len(id.Bucket) == 0 {
		return ""
	}
	keys := make([]string, 0, len(id.Bucket))
	for k := range id.Bucket {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s\x00%s", k, id.Bucket[k])
	}
	return CanonicalBucketKey(b.String())
}

// BucketState holds the runtime state for a single rate-limit bucket.
type BucketState struct {
	// Id is the original BucketId from the proxy.
	Id *rlqspb.BucketId

	// NumRequestsAllowed tracks allowed requests since last report.
	NumRequestsAllowed uint64
	// NumRequestsDenied tracks denied requests since last report.
	NumRequestsDenied uint64

	// LastReport is the time the last usage report was sent for this bucket.
	LastReport time.Time
	// CreatedAt is when this bucket was first observed.
	CreatedAt time.Time
}

// BucketStore is a thread-safe in-memory store for rate-limit bucket states,
// keyed by canonicalized BucketId.
type BucketStore struct {
	mu      sync.RWMutex
	buckets map[CanonicalBucketKey]*BucketState
}

// NewBucketStore returns an initialized BucketStore.
func NewBucketStore() *BucketStore {
	return &BucketStore{
		buckets: make(map[CanonicalBucketKey]*BucketState),
	}
}

// Get returns the BucketState for the given BucketId, or nil if not present.
func (s *BucketStore) Get(id *rlqspb.BucketId) *BucketState {
	key := CanonicalizeBucketId(id)
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.buckets[key]
}

// GetByKey returns the BucketState for the given canonical key.
func (s *BucketStore) GetByKey(key CanonicalBucketKey) *BucketState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.buckets[key]
}

// GetOrCreate atomically returns an existing bucket or creates a new one.
// The created return value indicates whether a new bucket was inserted.
func (s *BucketStore) GetOrCreate(id *rlqspb.BucketId) (bucket *BucketState, created bool) {
	key := CanonicalizeBucketId(id)

	// Fast path: read lock.
	s.mu.RLock()
	if b, ok := s.buckets[key]; ok {
		s.mu.RUnlock()
		return b, false
	}
	s.mu.RUnlock()

	// Slow path: write lock, double-check.
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.buckets[key]; ok {
		return b, false
	}
	now := time.Now()
	b := &BucketState{
		Id:        id,
		CreatedAt: now,
	}
	s.buckets[key] = b
	return b, true
}

// Delete removes a bucket by BucketId. Returns true if it existed.
func (s *BucketStore) Delete(id *rlqspb.BucketId) bool {
	key := CanonicalizeBucketId(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.buckets[key]
	delete(s.buckets, key)
	return ok
}

// Len returns the number of buckets in the store.
func (s *BucketStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.buckets)
}

// ForEach calls fn for every bucket while holding a read lock.
// The callback must not modify the store.
func (s *BucketStore) ForEach(fn func(key CanonicalBucketKey, state *BucketState)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for k, v := range s.buckets {
		fn(k, v)
	}
}

// SnapshotAndReset atomically snapshots all buckets and resets their counters,
// returning a slice of copies suitable for building a usage report.
func (s *BucketStore) SnapshotAndReset() []BucketState {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	snaps := make([]BucketState, 0, len(s.buckets))
	for _, b := range s.buckets {
		snaps = append(snaps, *b)
		b.NumRequestsAllowed = 0
		b.NumRequestsDenied = 0
		b.LastReport = now
	}
	return snaps
}
