package storage

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	rlqspb "github.com/envoyproxy/go-control-plane/envoy/service/rate_limit_quota/v3"
)

func newTestRedis(t *testing.T) (*RedisStorage, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rs := NewRedisStorage(RedisConfig{
		Addr:   mr.Addr(),
		KeyTTL: 60 * time.Second,
	})
	t.Cleanup(func() { rs.Close() })
	return rs, mr
}

func TestRedisStorage_RecordUsage(t *testing.T) {
	rs, mr := newTestRedis(t)
	bid := &rlqspb.BucketId{Bucket: map[string]string{"name": "web"}}

	err := rs.RecordUsage(context.Background(), "test", UsageReport{
		BucketId:           bid,
		NumRequestsAllowed: 10,
		NumRequestsDenied:  2,
	})
	if err != nil {
		t.Fatal(err)
	}

	key := string(DomainScopedKey("test", BucketKeyFromProto(bid)))
	allowed := mr.HGet(key, fieldAllowed)
	if allowed != "10" {
		t.Fatalf("expected allowed=10, got %s", allowed)
	}
	denied := mr.HGet(key, fieldDenied)
	if denied != "2" {
		t.Fatalf("expected denied=2, got %s", denied)
	}
}

func TestRedisStorage_RecordUsage_Accumulates(t *testing.T) {
	rs, mr := newTestRedis(t)
	bid := &rlqspb.BucketId{Bucket: map[string]string{"name": "web"}}

	for range 3 {
		err := rs.RecordUsage(context.Background(), "test", UsageReport{
			BucketId:           bid,
			NumRequestsAllowed: 5,
			NumRequestsDenied:  1,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	key := string(DomainScopedKey("test", BucketKeyFromProto(bid)))
	allowed := mr.HGet(key, fieldAllowed)
	if allowed != "15" {
		t.Fatalf("expected allowed=15, got %s", allowed)
	}
	denied := mr.HGet(key, fieldDenied)
	if denied != "3" {
		t.Fatalf("expected denied=3, got %s", denied)
	}
}

func TestRedisStorage_RemoveBucket(t *testing.T) {
	rs, mr := newTestRedis(t)
	bid := &rlqspb.BucketId{Bucket: map[string]string{"name": "web"}}
	key := BucketKeyFromProto(bid)

	err := rs.RecordUsage(context.Background(), "test", UsageReport{
		BucketId:           bid,
		NumRequestsAllowed: 10,
		NumRequestsDenied:  2,
	})
	if err != nil {
		t.Fatal(err)
	}

	scopedKey := string(DomainScopedKey("test", key))
	if !mr.Exists(scopedKey) {
		t.Fatal("expected key to exist before removal")
	}

	err = rs.RemoveBucket(context.Background(), "test", key)
	if err != nil {
		t.Fatal(err)
	}

	if mr.Exists(scopedKey) {
		t.Fatal("expected key to be removed")
	}
}

func TestRedisStorage_DomainIsolation(t *testing.T) {
	rs, mr := newTestRedis(t)
	bid := &rlqspb.BucketId{Bucket: map[string]string{"name": "api"}}

	err := rs.RecordUsage(context.Background(), "domain1", UsageReport{
		BucketId:           bid,
		NumRequestsAllowed: 100,
		NumRequestsDenied:  10,
	})
	if err != nil {
		t.Fatal(err)
	}

	err = rs.RecordUsage(context.Background(), "domain2", UsageReport{
		BucketId:           bid,
		NumRequestsAllowed: 200,
		NumRequestsDenied:  20,
	})
	if err != nil {
		t.Fatal(err)
	}

	key := BucketKeyFromProto(bid)
	d1Key := string(DomainScopedKey("domain1", key))
	d2Key := string(DomainScopedKey("domain2", key))

	a1 := mr.HGet(d1Key, fieldAllowed)
	if a1 != "100" {
		t.Fatalf("expected domain1 allowed=100, got %s", a1)
	}
	a2 := mr.HGet(d2Key, fieldAllowed)
	if a2 != "200" {
		t.Fatalf("expected domain2 allowed=200, got %s", a2)
	}
}

func TestRedisStorage_KeyTTL(t *testing.T) {
	rs, mr := newTestRedis(t)
	bid := &rlqspb.BucketId{Bucket: map[string]string{"name": "web"}}

	err := rs.RecordUsage(context.Background(), "test", UsageReport{
		BucketId:           bid,
		NumRequestsAllowed: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	key := string(DomainScopedKey("test", BucketKeyFromProto(bid)))
	ttl := mr.TTL(key)
	if ttl <= 0 {
		t.Fatalf("expected positive TTL, got %v", ttl)
	}
	if ttl > 60*time.Second {
		t.Fatalf("expected TTL <= 60s, got %v", ttl)
	}
}

func TestRedisStorage_CircuitBreaker_FallsBackToMemory(t *testing.T) {
	rs, mr := newTestRedis(t)
	bid := &rlqspb.BucketId{Bucket: map[string]string{"name": "web"}}

	// Inject command errors to trigger circuit breaker without closing the connection.
	mr.SetError("LOADING Redis is loading")

	// Need consecutive failures to trip the breaker (default threshold = 3).
	for range defaultFailThreshold {
		err := rs.RecordUsage(context.Background(), "test", UsageReport{
			BucketId:           bid,
			NumRequestsAllowed: 10,
			NumRequestsDenied:  2,
		})
		if err != nil {
			t.Fatalf("expected fail-open, got error: %v", err)
		}
	}

	if !rs.isTripped() {
		t.Fatal("expected circuit breaker to be tripped")
	}

	// Verify data went to fallback (accumulated from all calls).
	key := BucketKeyFromProto(bid)
	scopedKey := DomainScopedKey("test", key)
	state, ok := rs.fallback.Get(scopedKey)
	if !ok {
		t.Fatal("expected fallback to have the data")
	}
	if state.Allowed != 10*uint64(defaultFailThreshold) || state.Denied != 2*uint64(defaultFailThreshold) {
		t.Fatalf("unexpected fallback state: %+v", state)
	}
}

func TestRedisStorage_CircuitBreaker_SingleErrorDoesNotTrip(t *testing.T) {
	rs, mr := newTestRedis(t)
	bid := &rlqspb.BucketId{Bucket: map[string]string{"name": "web"}}

	// Inject a command error.
	mr.SetError("ERR temporary")

	// A single failure should NOT trip the breaker.
	_ = rs.RecordUsage(context.Background(), "test", UsageReport{
		BucketId:           bid,
		NumRequestsAllowed: 1,
	})

	if rs.isTripped() {
		t.Fatal("circuit breaker should not trip on a single error")
	}
}

func TestRedisStorage_CircuitBreaker_SuccessResetsCounter(t *testing.T) {
	rs, mr := newTestRedis(t)
	bid := &rlqspb.BucketId{Bucket: map[string]string{"name": "web"}}

	// Cause failures just below threshold.
	mr.SetError("ERR temporary")
	for range defaultFailThreshold - 1 {
		_ = rs.RecordUsage(context.Background(), "test", UsageReport{
			BucketId:           bid,
			NumRequestsAllowed: 1,
		})
	}
	if rs.isTripped() {
		t.Fatal("should not be tripped yet")
	}

	// Clear error — a success resets the counter.
	mr.SetError("")
	err := rs.RecordUsage(context.Background(), "test", UsageReport{
		BucketId:           bid,
		NumRequestsAllowed: 1,
	})
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}

	// Cause failures again — should need full threshold again.
	mr.SetError("ERR temporary")
	for range defaultFailThreshold - 1 {
		_ = rs.RecordUsage(context.Background(), "test", UsageReport{
			BucketId:           bid,
			NumRequestsAllowed: 1,
		})
	}
	if rs.isTripped() {
		t.Fatal("should not trip because success reset the counter")
	}
}

func TestRedisStorage_CircuitBreaker_RecoverAfterRetry(t *testing.T) {
	rs, mr := newTestRedis(t)
	rs.retryInterval = 0 // Instant retry for testing.

	bid := &rlqspb.BucketId{Bucket: map[string]string{"name": "web"}}

	// Trip the breaker via SetError (avoids slow dial retries).
	mr.SetError("ERR connection lost")
	for range defaultFailThreshold {
		_ = rs.RecordUsage(context.Background(), "trip", UsageReport{
			BucketId:           bid,
			NumRequestsAllowed: 1,
		})
	}
	if !rs.isTripped() {
		t.Fatal("expected tripped")
	}

	// Clear the error to simulate Redis recovery.
	mr.SetError("")

	// Next call should recover.
	err := rs.RecordUsage(context.Background(), "test", UsageReport{
		BucketId:           bid,
		NumRequestsAllowed: 10,
	})
	if err != nil {
		t.Fatalf("expected recovery, got: %v", err)
	}
	if rs.isTripped() {
		t.Fatal("expected circuit breaker to be reset after recovery")
	}

	// Verify data went to Redis.
	key := string(DomainScopedKey("test", BucketKeyFromProto(bid)))
	allowed := mr.HGet(key, fieldAllowed)
	if allowed != "10" {
		t.Fatalf("expected allowed=10 in Redis, got %s", allowed)
	}
}

func TestRedisStorage_Ping(t *testing.T) {
	rs, _ := newTestRedis(t)
	if err := rs.Ping(context.Background()); err != nil {
		t.Fatalf("expected ping to succeed, got: %v", err)
	}
}

func TestRedisStorage_Ping_Failure(t *testing.T) {
	mr := miniredis.RunT(t)
	rs := NewRedisStorage(RedisConfig{
		Addr:   mr.Addr(),
		KeyTTL: 60 * time.Second,
	})
	t.Cleanup(func() { rs.Close() })

	mr.Close()
	if err := rs.Ping(context.Background()); err == nil {
		t.Fatal("expected ping to fail when Redis is down")
	}
}
