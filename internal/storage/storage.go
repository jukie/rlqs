package storage

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	rlqspb "github.com/envoyproxy/go-control-plane/envoy/service/rate_limit_quota/v3"
)

// BucketKey is a canonical string representation of a BucketId, used as a map key.
type BucketKey string

// BucketKeyFromProto produces a canonical, order-independent key from a BucketId proto.
// Keys are sorted lexicographically and joined with null-byte separators to avoid
// collisions with real data. Format: "key1\x00val1\x1ekey2\x00val2"
func BucketKeyFromProto(id *rlqspb.BucketId) BucketKey {
	if id == nil || len(id.GetBucket()) == 0 {
		return ""
	}
	keys := make([]string, 0, len(id.GetBucket()))
	for k := range id.GetBucket() {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte('\x1e') // record separator between pairs
		}
		sb.WriteString(k)
		sb.WriteByte('\x00') // null between key and value
		sb.WriteString(id.GetBucket()[k])
	}
	return BucketKey(sb.String())
}

// DomainScopedKey prefixes a bucket key with its domain to prevent collisions
// across domains. Format: "domain\x1fbucketKey"
func DomainScopedKey(domain string, key BucketKey) BucketKey {
	if domain == "" {
		return key
	}
	var sb strings.Builder
	sb.WriteString(domain)
	sb.WriteByte('\x1f') // unit separator between domain and bucket key
	sb.WriteString(string(key))
	return BucketKey(sb.String())
}

// UsageReport captures a single bucket's usage data from a client report.
type UsageReport struct {
	BucketId           *rlqspb.BucketId
	NumRequestsAllowed uint64
	NumRequestsDenied  uint64
	// TimeElapsed is the duration since the last report for this bucket.
	// Per the RLQS spec this is a required field on BucketQuotaUsage.
	TimeElapsed time.Duration
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

// RecordUsage implements BucketStore by accumulating usage into the in-memory store.
func (s *MemoryStorage) RecordUsage(_ context.Context, domain string, report UsageReport) error {
	key := BucketKeyFromProto(report.BucketId)
	scopedKey := DomainScopedKey(domain, key)
	s.Update(scopedKey, report.NumRequestsAllowed, report.NumRequestsDenied)
	return nil
}

// RemoveBucket implements BucketStore by removing the bucket from the in-memory store.
func (s *MemoryStorage) RemoveBucket(_ context.Context, domain string, key BucketKey) error {
	scopedKey := DomainScopedKey(domain, key)
	s.Reset(scopedKey)
	return nil
}

// Len returns the number of buckets in the store.
func (s *MemoryStorage) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.buckets)
}

// Clear removes all entries from the store.
func (s *MemoryStorage) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buckets = make(map[BucketKey]*UsageState)
}
