package storage

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// RedisConfig holds configuration for the Redis storage backend.
type RedisConfig struct {
	Addr            string
	PoolSize        int
	KeyTTL          time.Duration
	Logger          *zap.Logger
	MaxFallbackKeys int
}

// RedisStorage implements BucketStore backed by Redis.
// It uses HINCRBY for atomic counter updates and EXPIRE for auto-cleanup.
// A circuit breaker falls back to in-memory storage when Redis is unreachable.
// On recovery, accumulated fallback data is flushed to Redis.
type RedisStorage struct {
	client          *redis.Client
	fallback        *MemoryStorage
	keyTTL          time.Duration
	logger          *zap.Logger
	maxFallbackKeys int

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
	defaultMaxFallback   = 10000
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
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	maxFallback := cfg.MaxFallbackKeys
	if maxFallback <= 0 {
		maxFallback = defaultMaxFallback
	}

	client := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		PoolSize: poolSize,
	})

	return &RedisStorage{
		client:          client,
		fallback:        NewMemoryStorage(),
		keyTTL:          ttl,
		logger:          logger,
		maxFallbackKeys: maxFallback,
		retryInterval:   defaultRetryInterval,
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
				s.recoverFromFallback(ctx)
				return nil
			}
		}
		if s.maxFallbackKeys > 0 && s.fallback.Len() >= s.maxFallbackKeys {
			// Only drop if this would create a new entry; updates to existing keys are allowed.
			if _, exists := s.fallback.Get(DomainScopedKey(domain, key)); !exists {
				s.logger.Warn("fallback store at capacity, dropping usage report",
					zap.Int("max_keys", s.maxFallbackKeys))
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
				s.recoverFromFallback(ctx)
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

// recoverFromFallback flushes accumulated fallback data to Redis and resets the circuit breaker.
func (s *RedisStorage) recoverFromFallback(ctx context.Context) {
	s.flushFallback(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tripped.Store(false)
}

// flushFallback migrates accumulated in-memory fallback data to Redis.
// On failure, logs a warning documenting the data loss. The fallback store
// is always cleared afterward to prevent stale data on the next outage.
func (s *RedisStorage) flushFallback(ctx context.Context) {
	entries := s.fallback.All()
	if len(entries) == 0 {
		return
	}

	pipe := s.client.Pipeline()
	for key, state := range entries {
		k := string(key)
		if state.Allowed > 0 {
			pipe.HIncrBy(ctx, k, fieldAllowed, int64(state.Allowed))
		}
		if state.Denied > 0 {
			pipe.HIncrBy(ctx, k, fieldDenied, int64(state.Denied))
		}
		pipe.Expire(ctx, k, s.keyTTL)
	}

	_, err := pipe.Exec(ctx)
	if err != nil {
		s.logger.Warn("failed to flush fallback data to Redis on recovery, data lost",
			zap.Int("entries", len(entries)),
			zap.Error(err))
	} else {
		s.logger.Info("flushed fallback data to Redis on recovery",
			zap.Int("entries", len(entries)))
	}

	s.fallback.Clear()
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

func (s *RedisStorage) shouldRetry() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return time.Since(s.lastAttempt) >= s.retryInterval
}
