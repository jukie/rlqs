package quota

import (
	"sync"
	"testing"
	"time"

	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// fakeClock is a manually-controlled clock for deterministic tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(t time.Time) *fakeClock {
	return &fakeClock{now: t}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func blanketRule(rule typev3.RateLimitStrategy_BlanketRule) *typev3.RateLimitStrategy {
	return &typev3.RateLimitStrategy{
		Strategy: &typev3.RateLimitStrategy_BlanketRule_{
			BlanketRule: rule,
		},
	}
}

func requestsPerTimeUnit(rps uint64, unit typev3.RateLimitUnit) *typev3.RateLimitStrategy {
	return &typev3.RateLimitStrategy{
		Strategy: &typev3.RateLimitStrategy_RequestsPerTimeUnit_{
			RequestsPerTimeUnit: &typev3.RateLimitStrategy_RequestsPerTimeUnit{
				RequestsPerTimeUnit: rps,
				TimeUnit:            unit,
			},
		},
	}
}

func tokenBucket(maxTokens uint32, tokensPerFill uint32, fillInterval time.Duration) *typev3.RateLimitStrategy {
	return &typev3.RateLimitStrategy{
		Strategy: &typev3.RateLimitStrategy_TokenBucket{
			TokenBucket: &typev3.TokenBucket{
				MaxTokens:     maxTokens,
				TokensPerFill: wrapperspb.UInt32(tokensPerFill),
				FillInterval:  durationpb.New(fillInterval),
			},
		},
	}
}

func TestBlanketRuleAllowAll(t *testing.T) {
	clk := newFakeClock(time.Now())
	e := NewWithClock(Config{}, clk)
	e.Assign("b1", blanketRule(typev3.RateLimitStrategy_ALLOW_ALL))

	for i := 0; i < 100; i++ {
		ok, err := e.Allow("b1")
		require.NoError(t, err)
		assert.True(t, ok, "ALLOW_ALL should always allow")
	}
}

func TestBlanketRuleDenyAll(t *testing.T) {
	clk := newFakeClock(time.Now())
	e := NewWithClock(Config{}, clk)
	e.Assign("b1", blanketRule(typev3.RateLimitStrategy_DENY_ALL))

	for i := 0; i < 10; i++ {
		ok, err := e.Allow("b1")
		require.NoError(t, err)
		assert.False(t, ok, "DENY_ALL should always deny")
	}
}

func TestRequestsPerTimeUnitBasic(t *testing.T) {
	clk := newFakeClock(time.Now())
	e := NewWithClock(Config{}, clk)
	e.Assign("b1", requestsPerTimeUnit(5, typev3.RateLimitUnit_SECOND))

	// First 5 requests should be allowed.
	for i := 0; i < 5; i++ {
		ok, err := e.Allow("b1")
		require.NoError(t, err)
		assert.True(t, ok, "request %d should be allowed", i+1)
	}

	// 6th request should be denied.
	ok, err := e.Allow("b1")
	require.NoError(t, err)
	assert.False(t, ok, "request over limit should be denied")
}

func TestRequestsPerTimeUnitWindowReset(t *testing.T) {
	clk := newFakeClock(time.Now())
	e := NewWithClock(Config{}, clk)
	e.Assign("b1", requestsPerTimeUnit(3, typev3.RateLimitUnit_SECOND))

	// Exhaust the window.
	for i := 0; i < 3; i++ {
		ok, err := e.Allow("b1")
		require.NoError(t, err)
		assert.True(t, ok)
	}
	ok, err := e.Allow("b1")
	require.NoError(t, err)
	assert.False(t, ok, "should be denied after exhausting window")

	// Advance past the window.
	clk.Advance(time.Second)

	// Should allow again.
	for i := 0; i < 3; i++ {
		ok, err := e.Allow("b1")
		require.NoError(t, err)
		assert.True(t, ok, "request %d after reset should be allowed", i+1)
	}
}

func TestRequestsPerTimeUnitZero(t *testing.T) {
	clk := newFakeClock(time.Now())
	e := NewWithClock(Config{}, clk)
	e.Assign("b1", requestsPerTimeUnit(0, typev3.RateLimitUnit_SECOND))

	ok, err := e.Allow("b1")
	require.NoError(t, err)
	assert.False(t, ok, "zero requests_per_time_unit should deny all")
}

func TestTokenBucketBasic(t *testing.T) {
	clk := newFakeClock(time.Now())
	e := NewWithClock(Config{}, clk)
	e.Assign("b1", tokenBucket(3, 1, time.Second))

	// 3 tokens available initially.
	for i := 0; i < 3; i++ {
		ok, err := e.Allow("b1")
		require.NoError(t, err)
		assert.True(t, ok, "token %d should be available", i+1)
	}

	// No tokens left.
	ok, err := e.Allow("b1")
	require.NoError(t, err)
	assert.False(t, ok, "should be denied when tokens exhausted")
}

func TestTokenBucketRefill(t *testing.T) {
	clk := newFakeClock(time.Now())
	e := NewWithClock(Config{}, clk)
	e.Assign("b1", tokenBucket(5, 2, time.Second))

	// Drain all 5 tokens.
	for i := 0; i < 5; i++ {
		ok, err := e.Allow("b1")
		require.NoError(t, err)
		assert.True(t, ok)
	}
	ok, err := e.Allow("b1")
	require.NoError(t, err)
	assert.False(t, ok)

	// Advance 1 second -> 2 tokens refilled.
	clk.Advance(time.Second)

	for i := 0; i < 2; i++ {
		ok, err := e.Allow("b1")
		require.NoError(t, err)
		assert.True(t, ok, "refilled token %d should be available", i+1)
	}
	ok, err = e.Allow("b1")
	require.NoError(t, err)
	assert.False(t, ok, "should be denied after consuming refilled tokens")
}

func TestTokenBucketMaxCap(t *testing.T) {
	clk := newFakeClock(time.Now())
	e := NewWithClock(Config{}, clk)
	e.Assign("b1", tokenBucket(3, 10, time.Second))

	// Advance 10 seconds -> would add 100 tokens, but capped at 3.
	clk.Advance(10 * time.Second)

	for i := 0; i < 3; i++ {
		ok, err := e.Allow("b1")
		require.NoError(t, err)
		assert.True(t, ok)
	}
	ok, err := e.Allow("b1")
	require.NoError(t, err)
	assert.False(t, ok, "should be capped at max_tokens")
}

func TestTokenBucketDefaultTokensPerFill(t *testing.T) {
	// TokensPerFill not set should default to 1.
	strategy := &typev3.RateLimitStrategy{
		Strategy: &typev3.RateLimitStrategy_TokenBucket{
			TokenBucket: &typev3.TokenBucket{
				MaxTokens:    2,
				FillInterval: durationpb.New(time.Second),
				// TokensPerFill intentionally nil.
			},
		},
	}

	clk := newFakeClock(time.Now())
	e := NewWithClock(Config{}, clk)
	e.Assign("b1", strategy)

	// Drain 2 tokens.
	for i := 0; i < 2; i++ {
		ok, err := e.Allow("b1")
		require.NoError(t, err)
		assert.True(t, ok)
	}
	ok, err := e.Allow("b1")
	require.NoError(t, err)
	assert.False(t, ok)

	// Advance 1 second -> 1 token refilled (default).
	clk.Advance(time.Second)
	ok, err = e.Allow("b1")
	require.NoError(t, err)
	assert.True(t, ok, "should get 1 token with default tokens_per_fill")

	ok, err = e.Allow("b1")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestAssignmentTTL(t *testing.T) {
	clk := newFakeClock(time.Now())
	e := NewWithClock(Config{AssignmentTTL: 5 * time.Second}, clk)
	e.Assign("b1", blanketRule(typev3.RateLimitStrategy_ALLOW_ALL))

	// Should work within TTL.
	ok, err := e.Allow("b1")
	require.NoError(t, err)
	assert.True(t, ok)

	// Advance past TTL.
	clk.Advance(6 * time.Second)

	_, err = e.Allow("b1")
	assert.Error(t, err, "should error after TTL expires")
	assert.Contains(t, err.Error(), "expired")
}

func TestAssignmentTTLViaGet(t *testing.T) {
	clk := newFakeClock(time.Now())
	e := NewWithClock(Config{AssignmentTTL: 5 * time.Second}, clk)
	e.Assign("b1", blanketRule(typev3.RateLimitStrategy_ALLOW_ALL))

	assert.NotNil(t, e.Get("b1"))

	clk.Advance(6 * time.Second)
	assert.Nil(t, e.Get("b1"), "Get should return nil for expired bucket")
}

func TestAbandonmentTimeout(t *testing.T) {
	clk := newFakeClock(time.Now())
	e := NewWithClock(Config{AbandonmentTimeout: 10 * time.Second}, clk)
	e.Assign("b1", blanketRule(typev3.RateLimitStrategy_ALLOW_ALL))

	// Should work initially.
	ok, err := e.Allow("b1")
	require.NoError(t, err)
	assert.True(t, ok)

	// Advance past abandonment timeout without any activity.
	clk.Advance(11 * time.Second)

	_, err = e.Allow("b1")
	assert.Error(t, err, "should error after abandonment timeout")
	assert.Contains(t, err.Error(), "abandoned")
}

func TestAbandonmentTimeoutResetByAllow(t *testing.T) {
	clk := newFakeClock(time.Now())
	e := NewWithClock(Config{AbandonmentTimeout: 10 * time.Second}, clk)
	e.Assign("b1", blanketRule(typev3.RateLimitStrategy_ALLOW_ALL))

	// Keep touching via Allow within the timeout.
	for i := 0; i < 5; i++ {
		clk.Advance(8 * time.Second)
		ok, err := e.Allow("b1")
		require.NoError(t, err)
		assert.True(t, ok, "should still be active when accessed within timeout")
	}
}

func TestAbandonmentTimeoutResetByTouch(t *testing.T) {
	clk := newFakeClock(time.Now())
	e := NewWithClock(Config{AbandonmentTimeout: 10 * time.Second}, clk)
	e.Assign("b1", blanketRule(typev3.RateLimitStrategy_ALLOW_ALL))

	clk.Advance(8 * time.Second)
	assert.True(t, e.Touch("b1"))

	clk.Advance(8 * time.Second)
	// 16s total from assign, but only 8s from last touch -> still active.
	ok, err := e.Allow("b1")
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestTouchNonexistent(t *testing.T) {
	e := New(Config{})
	assert.False(t, e.Touch("nonexistent"))
}

func TestCleanup(t *testing.T) {
	clk := newFakeClock(time.Now())
	e := NewWithClock(Config{
		AssignmentTTL:      5 * time.Second,
		AbandonmentTimeout: 10 * time.Second,
	}, clk)

	e.Assign("expired", blanketRule(typev3.RateLimitStrategy_ALLOW_ALL))
	e.Assign("abandoned", blanketRule(typev3.RateLimitStrategy_ALLOW_ALL))
	e.Assign("active", blanketRule(typev3.RateLimitStrategy_ALLOW_ALL))

	// Advance 6s: "expired" and "abandoned" assigned at t=0 are past TTL.
	// Reassign "active" at t=6s.
	clk.Advance(6 * time.Second)
	e.Assign("active", blanketRule(typev3.RateLimitStrategy_ALLOW_ALL))

	removed := e.Cleanup()
	assert.Equal(t, 2, removed)
	assert.Equal(t, 1, e.Len())
	assert.NotNil(t, e.Get("active"))
}

func TestAllowNoBucket(t *testing.T) {
	e := New(Config{})
	_, err := e.Allow("nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no assignment")
}

func TestNilStrategy(t *testing.T) {
	clk := newFakeClock(time.Now())
	e := NewWithClock(Config{}, clk)
	e.Assign("b1", &typev3.RateLimitStrategy{})

	ok, err := e.Allow("b1")
	require.NoError(t, err)
	assert.True(t, ok, "nil strategy should default to allow")
}

func TestAssignReplaces(t *testing.T) {
	clk := newFakeClock(time.Now())
	e := NewWithClock(Config{}, clk)

	e.Assign("b1", blanketRule(typev3.RateLimitStrategy_DENY_ALL))
	ok, err := e.Allow("b1")
	require.NoError(t, err)
	assert.False(t, ok)

	// Replace with ALLOW_ALL.
	e.Assign("b1", blanketRule(typev3.RateLimitStrategy_ALLOW_ALL))
	ok, err = e.Allow("b1")
	require.NoError(t, err)
	assert.True(t, ok, "reassigned strategy should take effect")
}

func TestLen(t *testing.T) {
	e := New(Config{})
	assert.Equal(t, 0, e.Len())

	e.Assign("a", blanketRule(typev3.RateLimitStrategy_ALLOW_ALL))
	e.Assign("b", blanketRule(typev3.RateLimitStrategy_ALLOW_ALL))
	assert.Equal(t, 2, e.Len())

	e.Assign("c", blanketRule(typev3.RateLimitStrategy_ALLOW_ALL))
	assert.Equal(t, 3, e.Len())
}

func TestRequestsPerTimeUnitMinuteWindow(t *testing.T) {
	clk := newFakeClock(time.Now())
	e := NewWithClock(Config{}, clk)
	e.Assign("b1", requestsPerTimeUnit(2, typev3.RateLimitUnit_MINUTE))

	ok, err := e.Allow("b1")
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = e.Allow("b1")
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = e.Allow("b1")
	require.NoError(t, err)
	assert.False(t, ok)

	// 30 seconds isn't enough.
	clk.Advance(30 * time.Second)
	ok, err = e.Allow("b1")
	require.NoError(t, err)
	assert.False(t, ok, "should still be denied within minute window")

	// Full minute resets.
	clk.Advance(31 * time.Second)
	ok, err = e.Allow("b1")
	require.NoError(t, err)
	assert.True(t, ok, "should allow after minute window resets")
}

func TestTokenBucketPartialRefill(t *testing.T) {
	clk := newFakeClock(time.Now())
	e := NewWithClock(Config{}, clk)
	e.Assign("b1", tokenBucket(10, 4, time.Second))

	// Drain all 10 tokens.
	for i := 0; i < 10; i++ {
		ok, err := e.Allow("b1")
		require.NoError(t, err)
		assert.True(t, ok)
	}

	// Advance 500ms -> 2 tokens (4 per second * 0.5s).
	clk.Advance(500 * time.Millisecond)

	ok, err := e.Allow("b1")
	require.NoError(t, err)
	assert.True(t, ok, "should have partial refill token")

	ok, err = e.Allow("b1")
	require.NoError(t, err)
	assert.True(t, ok, "should have second partial refill token")

	ok, err = e.Allow("b1")
	require.NoError(t, err)
	assert.False(t, ok, "should be denied after consuming partial refill")
}

func TestConcurrentAccess(t *testing.T) {
	e := New(Config{})
	e.Assign("b1", tokenBucket(1000, 100, time.Millisecond))

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				e.Allow("b1")
			}
		}()
	}
	wg.Wait()
}
