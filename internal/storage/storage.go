package storage

import "sync"

// BucketKey is a canonical string representation of a bucket ID.
type BucketKey string

// BucketState tracks cumulative usage for a rate limit bucket.
type BucketState struct {
	Allowed uint64
	Denied  uint64
}

// Storage provides thread-safe bucket state access.
type Storage interface {
	Get(key BucketKey) (BucketState, bool)
	Update(key BucketKey, allowed, denied uint64)
	Reset(key BucketKey)
	All() map[BucketKey]BucketState
}

// MemoryStorage is an in-memory Storage implementation.
type MemoryStorage struct {
	mu      sync.RWMutex
	buckets map[BucketKey]*BucketState
}

func NewMemoryStorage() *MemoryStorage {
	return &MemoryStorage{
		buckets: make(map[BucketKey]*BucketState),
	}
}

func (s *MemoryStorage) Get(key BucketKey) (BucketState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.buckets[key]
	if !ok {
		return BucketState{}, false
	}
	return *state, true
}

func (s *MemoryStorage) Update(key BucketKey, allowed, denied uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.buckets[key]
	if !ok {
		s.buckets[key] = &BucketState{Allowed: allowed, Denied: denied}
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

func (s *MemoryStorage) All() map[BucketKey]BucketState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[BucketKey]BucketState, len(s.buckets))
	for k, v := range s.buckets {
		result[k] = *v
	}
	return result
}
