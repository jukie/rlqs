package storage

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jukie/rlqs/internal/metrics"
	"github.com/redis/go-redis/v9"
)

// RedisConfig holds configuration for the Redis storage backend.
type RedisConfig struct {
	Addr         string
	PoolSize     int
	KeyTTL       time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

// RedisStorage implements BucketStore backed by Redis.
// It uses HINCRBY for atomic counter updates and EXPIRE for auto-cleanup.
// A circuit breaker falls back to in-memory storage when Redis is unreachable.
// The breaker trips after consecutive failures (default 3) to avoid tripping on transient errors.
type RedisStorage struct {
	client   *redis.Client
	fallback *MemoryStorage
	keyTTL   time.Duration

	// Circuit breaker state.
	mu                sync.RWMutex
	tripped           atomic.Bool
	consecutiveErrors int64
	failureThreshold  int64
	lastAttempt       time.Time
	retryInterval     time.Duration
}

const (
	fieldAllowed = "allowed"
	fieldDenied  = "denied"

	defaultRetryInterval  = 5 * time.Second
	defaultKeyTTL         = 60 * time.Second
	defaultFailThreshold  = 3
	defaultReadTimeout    = 3 * time.Second
	defaultWriteTimeout   = 3 * time.Second
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
	readTimeout := cfg.ReadTimeout
	if readTimeout <= 0 {
		readTimeout = defaultReadTimeout
	}
	writeTimeout := cfg.WriteTimeout
	if writeTimeout <= 0 {
		writeTimeout = defaultWriteTimeout
	}

	client := redis.NewClient(&redis.Options{
		Addr:         cfg.Addr,
		PoolSize:     poolSize,
		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
	})

	return &RedisStorage{
		client:           client,
		fallback:         NewMemoryStorage(),
		keyTTL:           ttl,
		retryInterval:    defaultRetryInterval,
		failureThreshold: defaultFailThreshold,
	}
}

// Ping verifies connectivity to Redis. Use on startup to catch misconfigurations early.
func (s *RedisStorage) Ping(ctx context.Context) error {
	return s.client.Ping(ctx).Err()
}

// RecordUsage implements BucketStore using Redis HINCRBY for atomic counter updates.
// Falls back to in-memory storage if Redis is unreachable (fail-open).
func (s *RedisStorage) RecordUsage(ctx context.Context, domain string, report UsageReport) error {
	key := BucketKeyFromProto(report.BucketId)
	scopedKey := string(DomainScopedKey(domain, key))

	start := time.Now()
	defer func() {
		metrics.StorageOperationDuration.WithLabelValues("record_usage").Observe(time.Since(start).Seconds())
	}()

	if s.isTripped() {
		if s.shouldRetry() {
			if err := s.tryRedisRecordUsage(ctx, scopedKey, report); err == nil {
				s.reset()
				return nil
			}
			s.updateLastAttempt()
		}
		metrics.RedisFallbackOperationsTotal.Inc()
		return s.fallback.RecordUsage(ctx, domain, report)
	}

	if err := s.tryRedisRecordUsage(ctx, scopedKey, report); err != nil {
		s.recordFailure("record_usage", err)
		metrics.RedisFallbackOperationsTotal.Inc()
		return s.fallback.RecordUsage(ctx, domain, report)
	}
	s.recordSuccess()
	return nil
}

// RemoveBucket implements BucketStore by deleting the Redis key.
// Falls back to in-memory storage if Redis is unreachable.
func (s *RedisStorage) RemoveBucket(ctx context.Context, domain string, key BucketKey) error {
	scopedKey := string(DomainScopedKey(domain, key))

	start := time.Now()
	defer func() {
		metrics.StorageOperationDuration.WithLabelValues("remove_bucket").Observe(time.Since(start).Seconds())
	}()

	if s.isTripped() {
		if s.shouldRetry() {
			if err := s.client.Del(ctx, scopedKey).Err(); err == nil {
				s.reset()
				return nil
			}
			s.updateLastAttempt()
		}
		metrics.RedisFallbackOperationsTotal.Inc()
		return s.fallback.RemoveBucket(ctx, domain, key)
	}

	if err := s.client.Del(ctx, scopedKey).Err(); err != nil {
		s.recordFailure("remove_bucket", err)
		metrics.RedisFallbackOperationsTotal.Inc()
		return s.fallback.RemoveBucket(ctx, domain, key)
	}
	s.recordSuccess()
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

// recordFailure increments consecutive error count and trips the breaker if threshold is reached.
func (s *RedisStorage) recordFailure(operation string, err error) {
	errType := classifyError(err)
	metrics.StorageErrorsTotal.WithLabelValues(operation, errType).Inc()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.consecutiveErrors++
	if s.consecutiveErrors >= s.failureThreshold {
		s.tripped.Store(true)
		s.lastAttempt = time.Now()
		metrics.RedisCircuitBreakerState.Set(1)
	}
}

// recordSuccess resets the consecutive error counter on a successful operation.
func (s *RedisStorage) recordSuccess() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.consecutiveErrors > 0 {
		s.consecutiveErrors = 0
	}
}

func (s *RedisStorage) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tripped.Store(false)
	s.consecutiveErrors = 0
	metrics.RedisCircuitBreakerState.Set(0)
}

func (s *RedisStorage) updateLastAttempt() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastAttempt = time.Now()
}

func (s *RedisStorage) shouldRetry() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return time.Since(s.lastAttempt) >= s.retryInterval
}

// classifyError returns a short label for the type of Redis error.
func classifyError(err error) string {
	if err == nil {
		return "none"
	}
	switch {
	case redis.HasErrorPrefix(err, "READONLY"):
		return "readonly"
	case redis.HasErrorPrefix(err, "LOADING"):
		return "loading"
	case redis.HasErrorPrefix(err, "CLUSTERDOWN"):
		return "clusterdown"
	default:
		return "redis_error"
	}
}
