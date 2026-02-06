package storage

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisConfig holds configuration for the Redis storage backend.
type RedisConfig struct {
	Addr     string
	PoolSize int
	KeyTTL   time.Duration
}

// RedisStorage implements BucketStore backed by Redis.
// It uses HINCRBY for atomic counter updates and EXPIRE for auto-cleanup.
// A circuit breaker falls back to in-memory storage when Redis is unreachable.
type RedisStorage struct {
	client   *redis.Client
	fallback *MemoryStorage
	keyTTL   time.Duration

	// Circuit breaker state.
	mu            sync.RWMutex
	tripped       atomic.Bool
	lastAttempt   time.Time
	retryInterval time.Duration
}

const (
	fieldAllowed = "allowed"
	fieldDenied  = "denied"

	defaultRetryInterval = 5 * time.Second
	defaultKeyTTL        = 60 * time.Second
)

// NewRedisStorage creates a new Redis-backed BucketStore with circuit breaker fallback.
func NewRedisStorage(cfg RedisConfig) *RedisStorage {
	poolSize := cfg.PoolSize
	if poolSize <= 0 {
		poolSize = 10
	}
	ttl := cfg.KeyTTL
	if ttl <= 0 {
		ttl = defaultKeyTTL
	}

	client := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		PoolSize: poolSize,
	})

	return &RedisStorage{
		client:        client,
		fallback:      NewMemoryStorage(),
		keyTTL:        ttl,
		retryInterval: defaultRetryInterval,
	}
}

// RecordUsage implements BucketStore using Redis HINCRBY for atomic counter updates.
// Falls back to in-memory storage if Redis is unreachable (fail-open).
func (s *RedisStorage) RecordUsage(ctx context.Context, domain string, report UsageReport) error {
	key := BucketKeyFromProto(report.BucketId)
	scopedKey := string(DomainScopedKey(domain, key))

	if s.isTripped() {
		if s.shouldRetry() {
			if err := s.tryRedisRecordUsage(ctx, scopedKey, report); err == nil {
				s.reset()
				return nil
			}
		}
		return s.fallback.RecordUsage(ctx, domain, report)
	}

	if err := s.tryRedisRecordUsage(ctx, scopedKey, report); err != nil {
		s.trip()
		return s.fallback.RecordUsage(ctx, domain, report)
	}
	return nil
}

// RemoveBucket implements BucketStore by deleting the Redis key.
// Falls back to in-memory storage if Redis is unreachable.
func (s *RedisStorage) RemoveBucket(ctx context.Context, domain string, key BucketKey) error {
	scopedKey := string(DomainScopedKey(domain, key))

	if s.isTripped() {
		if s.shouldRetry() {
			if err := s.client.Del(ctx, scopedKey).Err(); err == nil {
				s.reset()
				return nil
			}
		}
		return s.fallback.RemoveBucket(ctx, domain, key)
	}

	if err := s.client.Del(ctx, scopedKey).Err(); err != nil {
		s.trip()
		return s.fallback.RemoveBucket(ctx, domain, key)
	}
	return nil
}

// Close shuts down the Redis client connection.
func (s *RedisStorage) Close() error {
	return s.client.Close()
}

func (s *RedisStorage) tryRedisRecordUsage(ctx context.Context, key string, report UsageReport) error {
	pipe := s.client.Pipeline()
	pipe.HIncrBy(ctx, key, fieldAllowed, int64(report.NumRequestsAllowed))
	pipe.HIncrBy(ctx, key, fieldDenied, int64(report.NumRequestsDenied))
	pipe.Expire(ctx, key, s.keyTTL)
	_, err := pipe.Exec(ctx)
	return err
}

func (s *RedisStorage) isTripped() bool {
	return s.tripped.Load()
}

func (s *RedisStorage) trip() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tripped.Store(true)
	s.lastAttempt = time.Now()
}

func (s *RedisStorage) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tripped.Store(false)
}

func (s *RedisStorage) shouldRetry() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return time.Since(s.lastAttempt) >= s.retryInterval
}
