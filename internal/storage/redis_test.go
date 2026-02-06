package storage

import (
	"context"
	"fmt"
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

	// Verify both the recovery call (10) and the flushed fallback data (5) are in Redis.
	key := string(DomainScopedKey("test", BucketKeyFromProto(bid)))
	allowed := mr.HGet(key, fieldAllowed)
	if allowed != "15" {
		t.Fatalf("expected allowed=15 (10 from recovery + 5 flushed from fallback), got %s", allowed)
	}

	// Verify fallback was cleared after flush.
	if rs.fallback.Len() != 0 {
		t.Fatalf("expected fallback to be empty after recovery, got %d entries", rs.fallback.Len())
	}
}

func TestRedisStorage_RecoverFlushesFallbackToRedis(t *testing.T) {
	mr := miniredis.RunT(t)
	rs := NewRedisStorage(RedisConfig{
		Addr:   mr.Addr(),
		KeyTTL: 60 * time.Second,
	})
	t.Cleanup(func() { rs.Close() })

	bid1 := &rlqspb.BucketId{Bucket: map[string]string{"name": "api"}}
	bid2 := &rlqspb.BucketId{Bucket: map[string]string{"name": "web"}}

	// Trip the breaker. First call discovers Redis is down.
	mr.Close()
	_ = rs.RecordUsage(context.Background(), "test", UsageReport{
		BucketId:           bid1,
		NumRequestsAllowed: 10,
		NumRequestsDenied:  1,
	})

	// Remaining calls go straight to fallback (retryInterval prevents Redis retries).
	_ = rs.RecordUsage(context.Background(), "test", UsageReport{
		BucketId:           bid1,
		NumRequestsAllowed: 10,
		NumRequestsDenied:  1,
	})
	_ = rs.RecordUsage(context.Background(), "test", UsageReport{
		BucketId:           bid1,
		NumRequestsAllowed: 10,
		NumRequestsDenied:  1,
	})
	for i := 0; i < 3; i++ {
		_ = rs.RecordUsage(context.Background(), "test", UsageReport{
			BucketId:           bid2,
			NumRequestsAllowed: 5,
			NumRequestsDenied:  2,
		})
	}

	if rs.fallback.Len() != 2 {
		t.Fatalf("expected 2 fallback entries, got %d", rs.fallback.Len())
	}

	// Restart Redis and enable instant retry for recovery.
	mr.Restart()
	rs.retryInterval = 0

	err := rs.RecordUsage(context.Background(), "test", UsageReport{
		BucketId:           bid1,
		NumRequestsAllowed: 1,
	})
	if err != nil {
		t.Fatalf("expected recovery, got: %v", err)
	}

	// Verify all fallback data was flushed to Redis.
	key1 := string(DomainScopedKey("test", BucketKeyFromProto(bid1)))
	key2 := string(DomainScopedKey("test", BucketKeyFromProto(bid2)))

	// bid1: 30 from fallback + 1 from recovery call = 31
	a1 := mr.HGet(key1, fieldAllowed)
	if a1 != "31" {
		t.Fatalf("expected bid1 allowed=31, got %s", a1)
	}
	d1 := mr.HGet(key1, fieldDenied)
	if d1 != "3" {
		t.Fatalf("expected bid1 denied=3, got %s", d1)
	}

	// bid2: 15 from fallback only
	a2 := mr.HGet(key2, fieldAllowed)
	if a2 != "15" {
		t.Fatalf("expected bid2 allowed=15, got %s", a2)
	}
	d2 := mr.HGet(key2, fieldDenied)
	if d2 != "6" {
		t.Fatalf("expected bid2 denied=6, got %s", d2)
	}

	// Fallback should be empty.
	if rs.fallback.Len() != 0 {
		t.Fatalf("expected fallback to be empty after flush, got %d", rs.fallback.Len())
	}
}

func TestRedisStorage_FallbackCapacity(t *testing.T) {
	mr := miniredis.RunT(t)
	rs := NewRedisStorage(RedisConfig{
		Addr:            mr.Addr(),
		KeyTTL:          60 * time.Second,
		MaxFallbackKeys: 3,
	})
	t.Cleanup(func() { rs.Close() })

	// Trip the breaker.
	mr.Close()

	// Fill fallback to capacity with distinct buckets.
	for i := 0; i < 3; i++ {
		bid := &rlqspb.BucketId{Bucket: map[string]string{"name": fmt.Sprintf("bucket%d", i)}}
		err := rs.RecordUsage(context.Background(), "test", UsageReport{
			BucketId:           bid,
			NumRequestsAllowed: 1,
		})
		if err != nil {
			t.Fatalf("expected no error for entry %d, got: %v", i, err)
		}
	}

	if rs.fallback.Len() != 3 {
		t.Fatalf("expected 3 fallback entries, got %d", rs.fallback.Len())
	}

	// Next distinct bucket should be dropped (no error, fail-open).
	bid := &rlqspb.BucketId{Bucket: map[string]string{"name": "overflow"}}
	err := rs.RecordUsage(context.Background(), "test", UsageReport{
		BucketId:           bid,
		NumRequestsAllowed: 100,
	})
	if err != nil {
		t.Fatalf("expected no error on overflow, got: %v", err)
	}

	// Fallback should still have 3 entries (overflow was dropped).
	if rs.fallback.Len() != 3 {
		t.Fatalf("expected 3 fallback entries after overflow, got %d", rs.fallback.Len())
	}

	// Existing buckets should still accept updates (same key, no new entry).
	existingBid := &rlqspb.BucketId{Bucket: map[string]string{"name": "bucket0"}}
	err = rs.RecordUsage(context.Background(), "test", UsageReport{
		BucketId:           existingBid,
		NumRequestsAllowed: 5,
	})
	if err != nil {
		t.Fatalf("expected update to existing key to succeed, got: %v", err)
	}

	// Verify bucket0 was updated (1 + 5 = 6).
	key := DomainScopedKey("test", BucketKeyFromProto(existingBid))
	state, ok := rs.fallback.Get(key)
	if !ok {
		t.Fatal("expected bucket0 to exist in fallback")
	}
	if state.Allowed != 6 {
		t.Fatalf("expected bucket0 allowed=6, got %d", state.Allowed)
	}
}

func TestRedisStorage_RemoveBucket_RecoverFlushesFallback(t *testing.T) {
	mr := miniredis.RunT(t)
	rs := NewRedisStorage(RedisConfig{
		Addr:   mr.Addr(),
		KeyTTL: 60 * time.Second,
	})
	t.Cleanup(func() { rs.Close() })

	bid := &rlqspb.BucketId{Bucket: map[string]string{"name": "web"}}
	key := BucketKeyFromProto(bid)

	// Accumulate data during outage.
	mr.Close()
	_ = rs.RecordUsage(context.Background(), "test", UsageReport{
		BucketId:           bid,
		NumRequestsAllowed: 10,
		NumRequestsDenied:  3,
	})

	if rs.fallback.Len() != 1 {
		t.Fatalf("expected 1 fallback entry, got %d", rs.fallback.Len())
	}

	// Restart and enable instant retry for recovery.
	mr.Restart()
	rs.retryInterval = 0
	err := rs.RemoveBucket(context.Background(), "test", key)
	if err != nil {
		t.Fatalf("expected recovery on RemoveBucket, got: %v", err)
	}

	if rs.isTripped() {
		t.Fatal("expected circuit breaker to be reset")
	}

	// Fallback should be cleared after flush.
	if rs.fallback.Len() != 0 {
		t.Fatalf("expected fallback to be empty after recovery, got %d", rs.fallback.Len())
	}

	// Verify flushed data is in Redis (the flush happens before the delete,
	// and the delete only removes the specific key being deleted, not the flushed keys).
	scopedKey := string(DomainScopedKey("test", key))
	a := mr.HGet(scopedKey, fieldAllowed)
	if a != "10" {
		t.Fatalf("expected flushed allowed=10, got %s", a)
	}
}
