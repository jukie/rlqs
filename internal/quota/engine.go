package quota

import (
	"fmt"
	"sync"
	"time"

	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
)

// Clock abstracts time for testability.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Config holds configuration for the Engine.
type Config struct {
	// AbandonmentTimeout is how long a bucket can go without activity
	// before being considered abandoned and removed.
	AbandonmentTimeout time.Duration

	// AssignmentTTL is the maximum lifetime of a quota assignment.
	// After this duration from assignment, the bucket expires.
	AssignmentTTL time.Duration
}

// Bucket holds the quota state for a single rate limit bucket.
type Bucket struct {
	Strategy     *typev3.RateLimitStrategy
	AssignedAt   time.Time
	LastActivity time.Time

	// token bucket state
	tokens     float64
	lastRefill time.Time

	// requests-per-time-unit state
	windowStart  time.Time
	requestCount uint64
}

// Engine manages quota assignments and evaluates rate limit strategies.
//
//nolint:revive // QuotaEngine is intentionally named to distinguish from the Engine interface
type QuotaEngine struct {
	mu                 sync.Mutex
	buckets            map[string]*Bucket
	abandonmentTimeout time.Duration
	assignmentTTL      time.Duration
	clock              Clock
}

// New creates a new Engine with the given configuration.
func New(cfg Config) *QuotaEngine {
	return &QuotaEngine{
		buckets:            make(map[string]*Bucket),
		abandonmentTimeout: cfg.AbandonmentTimeout,
		assignmentTTL:      cfg.AssignmentTTL,
		clock:              realClock{},
	}
}

// NewWithClock creates an Engine with an injected clock for testing.
func NewWithClock(cfg Config, clock Clock) *QuotaEngine {
	e := New(cfg)
	e.clock = clock
	return e
}

// Assign sets or replaces the quota strategy for a bucket.
// Per the RLQS spec, when an identical strategy is reassigned, the assignment
// duration is extended without resetting the rate limiting state (tokens, windows).
func (e *QuotaEngine) Assign(bucketID string, strategy *typev3.RateLimitStrategy) *Bucket {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := e.clock.Now()

	// If the bucket already exists with the same strategy type, extend TTL
	// without resetting rate-limit state (tokens, window counters).
	if existing, ok := e.buckets[bucketID]; ok && strategiesEqual(existing.Strategy, strategy) {
		existing.AssignedAt = now
		existing.LastActivity = now
		existing.Strategy = strategy // update in case fields changed slightly
		return existing
	}

	b := &Bucket{
		Strategy:     strategy,
		AssignedAt:   now,
		LastActivity: now,
		lastRefill:   now,
		windowStart:  now,
	}

	if tb := strategy.GetTokenBucket(); tb != nil {
		b.tokens = float64(tb.GetMaxTokens())
	}

	e.buckets[bucketID] = b
	return b
}

// strategiesEqual returns true if two strategies are of the same type with the
// same parameters, meaning a reassignment should extend TTL rather than reset state.
func strategiesEqual(a, b *typev3.RateLimitStrategy) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if a.Strategy == nil || b.Strategy == nil {
		return a.Strategy == nil && b.Strategy == nil
	}

	switch sa := a.Strategy.(type) {
	case *typev3.RateLimitStrategy_BlanketRule_:
		sb, ok := b.Strategy.(*typev3.RateLimitStrategy_BlanketRule_)
		return ok && sa.BlanketRule == sb.BlanketRule

	case *typev3.RateLimitStrategy_RequestsPerTimeUnit_:
		sb, ok := b.Strategy.(*typev3.RateLimitStrategy_RequestsPerTimeUnit_)
		if !ok {
			return false
		}
		return sa.RequestsPerTimeUnit.GetRequestsPerTimeUnit() == sb.RequestsPerTimeUnit.GetRequestsPerTimeUnit() &&
			sa.RequestsPerTimeUnit.GetTimeUnit() == sb.RequestsPerTimeUnit.GetTimeUnit()

	case *typev3.RateLimitStrategy_TokenBucket:
		sb, ok := b.Strategy.(*typev3.RateLimitStrategy_TokenBucket)
		if !ok {
			return false
		}
		return sa.TokenBucket.GetMaxTokens() == sb.TokenBucket.GetMaxTokens() &&
			sa.TokenBucket.GetTokensPerFill().GetValue() == sb.TokenBucket.GetTokensPerFill().GetValue() &&
			sa.TokenBucket.GetFillInterval().AsDuration() == sb.TokenBucket.GetFillInterval().AsDuration()

	default:
		return false
	}
}

// Get returns the bucket if it exists and is still valid.
// Returns nil if the bucket is not found, expired, or abandoned.
func (e *QuotaEngine) Get(bucketID string) *Bucket {
	e.mu.Lock()
	defer e.mu.Unlock()

	b, ok := e.buckets[bucketID]
	if !ok {
		return nil
	}

	now := e.clock.Now()
	if e.isExpired(b, now) || e.isAbandoned(b, now) {
		delete(e.buckets, bucketID)
		return nil
	}

	return b
}

// Touch updates the last activity time for a bucket.
func (e *QuotaEngine) Touch(bucketID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	b, ok := e.buckets[bucketID]
	if !ok {
		return false
	}
	b.LastActivity = e.clock.Now()
	return true
}

// Allow evaluates the assigned strategy for a bucket and returns whether
// the request should be allowed.
func (e *QuotaEngine) Allow(bucketID string) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	b, ok := e.buckets[bucketID]
	if !ok {
		return false, fmt.Errorf("quota: no assignment for bucket %q", bucketID)
	}

	now := e.clock.Now()
	if e.isExpired(b, now) {
		delete(e.buckets, bucketID)
		return false, fmt.Errorf("quota: assignment expired for bucket %q", bucketID)
	}
	if e.isAbandoned(b, now) {
		delete(e.buckets, bucketID)
		return false, fmt.Errorf("quota: bucket %q abandoned", bucketID)
	}

	b.LastActivity = now
	return e.evaluate(b, now)
}

// Cleanup removes expired and abandoned buckets. Returns the number removed.
func (e *QuotaEngine) Cleanup() int {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := e.clock.Now()
	removed := 0

	for id, b := range e.buckets {
		if e.isExpired(b, now) || e.isAbandoned(b, now) {
			delete(e.buckets, id)
			removed++
		}
	}

	return removed
}

// Len returns the number of active buckets.
func (e *QuotaEngine) Len() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.buckets)
}

func (e *QuotaEngine) isExpired(b *Bucket, now time.Time) bool {
	return e.assignmentTTL > 0 && now.Sub(b.AssignedAt) > e.assignmentTTL
}

func (e *QuotaEngine) isAbandoned(b *Bucket, now time.Time) bool {
	return e.abandonmentTimeout > 0 && now.Sub(b.LastActivity) > e.abandonmentTimeout
}

func (e *QuotaEngine) evaluate(b *Bucket, now time.Time) (bool, error) {
	if b.Strategy == nil || b.Strategy.Strategy == nil {
		return true, nil
	}

	switch s := b.Strategy.Strategy.(type) {
	case *typev3.RateLimitStrategy_BlanketRule_:
		return s.BlanketRule == typev3.RateLimitStrategy_ALLOW_ALL, nil

	case *typev3.RateLimitStrategy_RequestsPerTimeUnit_:
		return e.evaluateRequestsPerTimeUnit(b, s.RequestsPerTimeUnit, now)

	case *typev3.RateLimitStrategy_TokenBucket:
		return e.evaluateTokenBucket(b, s.TokenBucket, now)

	default:
		return false, fmt.Errorf("quota: unknown strategy type: %T", s)
	}
}

func (e *QuotaEngine) evaluateRequestsPerTimeUnit(
	b *Bucket,
	rptu *typev3.RateLimitStrategy_RequestsPerTimeUnit,
	now time.Time,
) (bool, error) {
	if rptu.GetRequestsPerTimeUnit() == 0 {
		return false, nil
	}

	window := rateLimitUnitToDuration(rptu.GetTimeUnit())
	if window == 0 {
		return false, fmt.Errorf("quota: unknown time unit: %v", rptu.GetTimeUnit())
	}

	if now.Sub(b.windowStart) >= window {
		b.windowStart = now
		b.requestCount = 0
	}

	if b.requestCount >= rptu.GetRequestsPerTimeUnit() {
		return false, nil
	}

	b.requestCount++
	return true, nil
}

func (e *QuotaEngine) evaluateTokenBucket(b *Bucket, tb *typev3.TokenBucket, now time.Time) (bool, error) {
	fillInterval := tb.GetFillInterval().AsDuration()
	if fillInterval > 0 {
		elapsed := now.Sub(b.lastRefill)
		fills := float64(elapsed) / float64(fillInterval)
		tokensPerFill := float64(1)
		if tb.GetTokensPerFill() != nil {
			tokensPerFill = float64(tb.GetTokensPerFill().GetValue())
		}
		b.tokens += fills * tokensPerFill
		maxTokens := float64(tb.GetMaxTokens())
		if b.tokens > maxTokens {
			b.tokens = maxTokens
		}
		b.lastRefill = now
	}

	if b.tokens < 1 {
		return false, nil
	}

	b.tokens--
	return true, nil
}

func rateLimitUnitToDuration(unit typev3.RateLimitUnit) time.Duration {
	switch unit {
	case typev3.RateLimitUnit_SECOND:
		return time.Second
	case typev3.RateLimitUnit_MINUTE:
		return time.Minute
	case typev3.RateLimitUnit_HOUR:
		return time.Hour
	case typev3.RateLimitUnit_DAY:
		return 24 * time.Hour
	case typev3.RateLimitUnit_MONTH:
		return 30 * 24 * time.Hour
	case typev3.RateLimitUnit_YEAR:
		return 365 * 24 * time.Hour
	default:
		return 0
	}
}
