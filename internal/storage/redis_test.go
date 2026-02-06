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

	for i := 0; i < 3; i++ {
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
	mr := miniredis.RunT(t)
	rs := NewRedisStorage(RedisConfig{
		Addr:   mr.Addr(),
		KeyTTL: 60 * time.Second,
	})
	t.Cleanup(func() { rs.Close() })

	bid := &rlqspb.BucketId{Bucket: map[string]string{"name": "web"}}

	// Stop Redis to trigger circuit breaker.
	mr.Close()

	// Should fall back to in-memory (fail-open).
	err := rs.RecordUsage(context.Background(), "test", UsageReport{
		BucketId:           bid,
		NumRequestsAllowed: 10,
		NumRequestsDenied:  2,
	})
	if err != nil {
		t.Fatalf("expected fail-open, got error: %v", err)
	}

	if !rs.isTripped() {
		t.Fatal("expected circuit breaker to be tripped")
	}

	// Verify data went to fallback.
	key := BucketKeyFromProto(bid)
	scopedKey := DomainScopedKey("test", key)
	state, ok := rs.fallback.Get(scopedKey)
	if !ok {
		t.Fatal("expected fallback to have the data")
	}
	if state.Allowed != 10 || state.Denied != 2 {
		t.Fatalf("unexpected fallback state: %+v", state)
	}
}

func TestRedisStorage_CircuitBreaker_RecoverAfterRetry(t *testing.T) {
	mr := miniredis.RunT(t)
	rs := NewRedisStorage(RedisConfig{
		Addr:   mr.Addr(),
		KeyTTL: 60 * time.Second,
	})
	rs.retryInterval = 0 // Instant retry for testing.
	t.Cleanup(func() { rs.Close() })

	bid := &rlqspb.BucketId{Bucket: map[string]string{"name": "web"}}

	// Trip the breaker.
	mr.Close()
	_ = rs.RecordUsage(context.Background(), "test", UsageReport{
		BucketId:           bid,
		NumRequestsAllowed: 5,
	})
	if !rs.isTripped() {
		t.Fatal("expected tripped")
	}

	// Restart Redis.
	mr.Restart()

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
