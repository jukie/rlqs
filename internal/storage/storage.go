package storage

import (
	"context"
	"sort"
	"strings"
	"sync"

	rlqspb "github.com/envoyproxy/go-control-plane/envoy/service/rate_limit_quota/v3"
)

// BucketKey is a canonical string representation of a BucketId, used as a map key.
type BucketKey string

// BucketKeyFromProto produces a canonical, order-independent key from a BucketId proto.
func BucketKeyFromProto(id *rlqspb.BucketId) BucketKey {
	if id == nil {
		return ""
	}
	pairs := make([]string, 0, len(id.GetBucket()))
	for k, v := range id.GetBucket() {
		pairs = append(pairs, k+"="+v)
	}
	sort.Strings(pairs)
	return BucketKey(strings.Join(pairs, "\x00"))
}

// UsageReport captures a single bucket's usage data from a client report.
type UsageReport struct {
	BucketId           *rlqspb.BucketId
	NumRequestsAllowed uint64
	NumRequestsDenied  uint64
}

// BucketStore persists and retrieves per-bucket quota state.
type BucketStore interface {
	// RecordUsage stores a usage report for the given domain and bucket.
	RecordUsage(ctx context.Context, domain string, report UsageReport) error

	// RemoveBucket deletes all state for the given domain and bucket.
	RemoveBucket(ctx context.Context, domain string, key BucketKey) error
}

// UsageState tracks cumulative usage for a rate limit bucket.
type UsageState struct {
	Allowed uint64
	Denied  uint64
}

// Storage provides thread-safe bucket state access.
type Storage interface {
	Get(key BucketKey) (UsageState, bool)
	Update(key BucketKey, allowed, denied uint64)
	Reset(key BucketKey)
	All() map[BucketKey]UsageState
}

// MemoryStorage is an in-memory Storage implementation.
type MemoryStorage struct {
	mu      sync.RWMutex
	buckets map[BucketKey]*UsageState
}

func NewMemoryStorage() *MemoryStorage {
	return &MemoryStorage{
		buckets: make(map[BucketKey]*UsageState),
	}
}

func (s *MemoryStorage) Get(key BucketKey) (UsageState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.buckets[key]
	if !ok {
		return UsageState{}, false
	}
	return *state, true
}

func (s *MemoryStorage) Update(key BucketKey, allowed, denied uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.buckets[key]
	if !ok {
		s.buckets[key] = &UsageState{Allowed: allowed, Denied: denied}
		return
	}
	state.Allowed += allowed
	state.Denied += denied
}

func (s *MemoryStorage) Reset(key BucketKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.buckets, key)
}

func (s *MemoryStorage) All() map[BucketKey]UsageState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[BucketKey]UsageState, len(s.buckets))
	for k, v := range s.buckets {
		result[k] = *v
	}
	return result
}
