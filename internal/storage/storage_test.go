package storage

import (
	"context"
	"testing"

	rlqspb "github.com/envoyproxy/go-control-plane/envoy/service/rate_limit_quota/v3"
)

func TestMemoryStorage_GetMissing(t *testing.T) {
	s := NewMemoryStorage()
	_, ok := s.Get("nonexistent")
	if ok {
		t.Fatal("expected not found")
	}
}

func TestMemoryStorage_UpdateAndGet(t *testing.T) {
	s := NewMemoryStorage()
	s.Update("bucket1", 10, 2)

	state, ok := s.Get("bucket1")
	if !ok {
		t.Fatal("expected found")
	}
	if state.Allowed != 10 || state.Denied != 2 {
		t.Fatalf("unexpected state: %+v", state)
	}

	s.Update("bucket1", 5, 1)
	state, _ = s.Get("bucket1")
	if state.Allowed != 15 || state.Denied != 3 {
		t.Fatalf("expected accumulated state, got: %+v", state)
	}
}

func TestMemoryStorage_Reset(t *testing.T) {
	s := NewMemoryStorage()
	s.Update("bucket1", 10, 2)
	s.Reset("bucket1")

	_, ok := s.Get("bucket1")
	if ok {
		t.Fatal("expected not found after reset")
	}
}

func TestMemoryStorage_All(t *testing.T) {
	s := NewMemoryStorage()
	s.Update("a", 1, 0)
	s.Update("b", 2, 0)

	all := s.All()
	if len(all) != 2 {
		t.Fatalf("expected 2 buckets, got %d", len(all))
	}
	if all["a"].Allowed != 1 {
		t.Fatalf("expected a.Allowed=1, got %d", all["a"].Allowed)
	}
	if all["b"].Allowed != 2 {
		t.Fatalf("expected b.Allowed=2, got %d", all["b"].Allowed)
	}
}

// --- BucketStore interface tests ---

func TestMemoryStorage_RecordUsage(t *testing.T) {
	s := NewMemoryStorage()
	bid := &rlqspb.BucketId{Bucket: map[string]string{"name": "web"}}

	err := s.RecordUsage(context.Background(), "test", UsageReport{
		BucketId:           bid,
		NumRequestsAllowed: 10,
		NumRequestsDenied:  2,
	})
	if err != nil {
		t.Fatal(err)
	}

	key := BucketKeyFromProto(bid)
	scopedKey := DomainScopedKey("test", key)
	state, ok := s.Get(scopedKey)
	if !ok {
		t.Fatal("expected bucket to exist")
	}
	if state.Allowed != 10 || state.Denied != 2 {
		t.Fatalf("unexpected state: %+v", state)
	}

	// Second RecordUsage should accumulate.
	err = s.RecordUsage(context.Background(), "test", UsageReport{
		BucketId:           bid,
		NumRequestsAllowed: 5,
		NumRequestsDenied:  1,
	})
	if err != nil {
		t.Fatal(err)
	}

	state, _ = s.Get(scopedKey)
	if state.Allowed != 15 || state.Denied != 3 {
		t.Fatalf("expected accumulated state (15,3), got: %+v", state)
	}
}

func TestMemoryStorage_RemoveBucket(t *testing.T) {
	s := NewMemoryStorage()
	bid := &rlqspb.BucketId{Bucket: map[string]string{"name": "web"}}
	key := BucketKeyFromProto(bid)

	// Use RecordUsage to store with domain scoping
	err := s.RecordUsage(context.Background(), "test", UsageReport{
		BucketId:           bid,
		NumRequestsAllowed: 10,
		NumRequestsDenied:  2,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify it was stored
	scopedKey := DomainScopedKey("test", key)
	if _, ok := s.Get(scopedKey); !ok {
		t.Fatal("expected bucket to exist before removal")
	}

	err = s.RemoveBucket(context.Background(), "test", key)
	if err != nil {
		t.Fatal(err)
	}

	_, ok := s.Get(scopedKey)
	if ok {
		t.Fatal("expected bucket to be removed")
	}
}

func TestMemoryStorage_DomainIsolation(t *testing.T) {
	s := NewMemoryStorage()
	bid := &rlqspb.BucketId{Bucket: map[string]string{"name": "api"}}

	// Record usage for domain1
	err := s.RecordUsage(context.Background(), "domain1", UsageReport{
		BucketId:           bid,
		NumRequestsAllowed: 100,
		NumRequestsDenied:  10,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Record usage for domain2 with same bucket ID
	err = s.RecordUsage(context.Background(), "domain2", UsageReport{
		BucketId:           bid,
		NumRequestsAllowed: 200,
		NumRequestsDenied:  20,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify they are stored separately
	key := BucketKeyFromProto(bid)
	domain1Key := DomainScopedKey("domain1", key)
	domain2Key := DomainScopedKey("domain2", key)

	state1, ok1 := s.Get(domain1Key)
	if !ok1 {
		t.Fatal("expected domain1 bucket to exist")
	}
	if state1.Allowed != 100 || state1.Denied != 10 {
		t.Fatalf("expected domain1 state (100,10), got: %+v", state1)
	}

	state2, ok2 := s.Get(domain2Key)
	if !ok2 {
		t.Fatal("expected domain2 bucket to exist")
	}
	if state2.Allowed != 200 || state2.Denied != 20 {
		t.Fatalf("expected domain2 state (200,20), got: %+v", state2)
	}
}

func TestBucketKeyFromProto_EmptyBucket(t *testing.T) {
	key := BucketKeyFromProto(&rlqspb.BucketId{Bucket: map[string]string{}})
	if key != "" {
		t.Fatalf("expected empty key for empty bucket map, got %q", key)
	}
}

func TestBucketKeyFromProto_OrderIndependent(t *testing.T) {
	a := BucketKeyFromProto(&rlqspb.BucketId{Bucket: map[string]string{"a": "1", "b": "2"}})
	b := BucketKeyFromProto(&rlqspb.BucketId{Bucket: map[string]string{"b": "2", "a": "1"}})
	if a != b {
		t.Fatalf("expected same key for order-different maps: %q vs %q", a, b)
	}
}
