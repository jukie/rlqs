package storage

import (
	"sync"
	"testing"

	rlqspb "github.com/envoyproxy/go-control-plane/envoy/service/rate_limit_quota/v3"
)

func bucketId(kvs ...string) *rlqspb.BucketId {
	m := make(map[string]string, len(kvs)/2)
	for i := 0; i < len(kvs)-1; i += 2 {
		m[kvs[i]] = kvs[i+1]
	}
	return &rlqspb.BucketId{Bucket: m}
}

// --- CanonicalizeBucketId ---

func TestCanonicalize_Nil(t *testing.T) {
	if got := CanonicalizeBucketId(nil); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestCanonicalize_Empty(t *testing.T) {
	if got := CanonicalizeBucketId(&rlqspb.BucketId{}); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestCanonicalize_SingleKey(t *testing.T) {
	a := CanonicalizeBucketId(bucketId("name", "foo"))
	b := CanonicalizeBucketId(bucketId("name", "foo"))
	if a != b {
		t.Fatalf("same inputs produced different keys: %q vs %q", a, b)
	}
}

func TestCanonicalize_OrderIndependent(t *testing.T) {
	// Two BucketIds with the same entries in different insertion order must
	// produce the same canonical key.
	a := CanonicalizeBucketId(bucketId("env", "prod", "service", "web", "path", "/api"))
	b := CanonicalizeBucketId(bucketId("service", "web", "path", "/api", "env", "prod"))
	c := CanonicalizeBucketId(bucketId("path", "/api", "env", "prod", "service", "web"))
	if a != b || b != c {
		t.Fatalf("order-dependent keys: %q, %q, %q", a, b, c)
	}
}

func TestCanonicalize_DifferentValues(t *testing.T) {
	a := CanonicalizeBucketId(bucketId("k", "v1"))
	b := CanonicalizeBucketId(bucketId("k", "v2"))
	if a == b {
		t.Fatal("different values produced same key")
	}
}

func TestCanonicalize_DifferentKeys(t *testing.T) {
	a := CanonicalizeBucketId(bucketId("k1", "v"))
	b := CanonicalizeBucketId(bucketId("k2", "v"))
	if a == b {
		t.Fatal("different keys produced same canonical key")
	}
}

func TestCanonicalize_NoAmbiguity(t *testing.T) {
	// "a"="b\nc" must differ from "a"="b","c"="" to avoid collisions.
	a := CanonicalizeBucketId(bucketId("a", "b\nc"))
	b := CanonicalizeBucketId(bucketId("a", "b", "c", ""))
	if a == b {
		t.Fatal("ambiguous canonicalization: newline in value collides with separator")
	}
}

// --- BucketStore basic CRUD ---

func TestStore_GetOrCreate_New(t *testing.T) {
	s := NewMemoryBucketStore()
	id := bucketId("svc", "web")
	b, created := s.GetOrCreate(id)
	if !created {
		t.Fatal("expected created=true for new bucket")
	}
	if b == nil {
		t.Fatal("returned nil bucket")
	}
	if b.Id != id {
		t.Fatal("bucket Id mismatch")
	}
	if b.CreatedAt.IsZero() {
		t.Fatal("CreatedAt not set")
	}
}

func TestStore_GetOrCreate_Existing(t *testing.T) {
	s := NewMemoryBucketStore()
	id := bucketId("svc", "web")
	b1, _ := s.GetOrCreate(id)
	b2, created := s.GetOrCreate(id)
	if created {
		t.Fatal("expected created=false for existing bucket")
	}
	if b1 != b2 {
		t.Fatal("returned different pointer for same bucket")
	}
}

func TestStore_GetOrCreate_OrderIndependent(t *testing.T) {
	s := NewMemoryBucketStore()
	id1 := bucketId("a", "1", "b", "2")
	id2 := bucketId("b", "2", "a", "1")
	b1, _ := s.GetOrCreate(id1)
	b2, created := s.GetOrCreate(id2)
	if created {
		t.Fatal("order-different BucketId should match existing bucket")
	}
	if b1 != b2 {
		t.Fatal("should return same pointer for order-different BucketIds")
	}
	if s.Len() != 1 {
		t.Fatalf("expected 1 bucket, got %d", s.Len())
	}
}

func TestStore_Get_Missing(t *testing.T) {
	s := NewMemoryBucketStore()
	if got := s.Get(bucketId("x", "y")); got != nil {
		t.Fatal("expected nil for missing bucket")
	}
}

func TestStore_Get_Present(t *testing.T) {
	s := NewMemoryBucketStore()
	id := bucketId("x", "y")
	want, _ := s.GetOrCreate(id)
	got := s.Get(id)
	if got != want {
		t.Fatal("Get returned wrong bucket")
	}
}

func TestStore_GetByKey(t *testing.T) {
	s := NewMemoryBucketStore()
	id := bucketId("a", "b")
	want, _ := s.GetOrCreate(id)
	key := CanonicalizeBucketId(id)
	got := s.GetByKey(key)
	if got != want {
		t.Fatal("GetByKey returned wrong bucket")
	}
}

func TestStore_Delete(t *testing.T) {
	s := NewMemoryBucketStore()
	id := bucketId("x", "y")
	s.GetOrCreate(id)
	if !s.Delete(id) {
		t.Fatal("Delete should return true for existing bucket")
	}
	if s.Len() != 0 {
		t.Fatal("bucket not removed")
	}
	if s.Delete(id) {
		t.Fatal("Delete should return false for missing bucket")
	}
}

func TestStore_Len(t *testing.T) {
	s := NewMemoryBucketStore()
	if s.Len() != 0 {
		t.Fatal("new store should be empty")
	}
	s.GetOrCreate(bucketId("a", "1"))
	s.GetOrCreate(bucketId("b", "2"))
	if s.Len() != 2 {
		t.Fatalf("expected 2, got %d", s.Len())
	}
}

// --- ForEach ---

func TestStore_ForEach(t *testing.T) {
	s := NewMemoryBucketStore()
	s.GetOrCreate(bucketId("a", "1"))
	s.GetOrCreate(bucketId("b", "2"))
	s.GetOrCreate(bucketId("c", "3"))

	visited := 0
	s.ForEach(func(_ CanonicalBucketKey, _ *BucketState) {
		visited++
	})
	if visited != 3 {
		t.Fatalf("expected 3 visits, got %d", visited)
	}
}

// --- SnapshotAndReset ---

func TestStore_SnapshotAndReset(t *testing.T) {
	s := NewMemoryBucketStore()
	b, _ := s.GetOrCreate(bucketId("svc", "web"))
	b.NumRequestsAllowed = 100
	b.NumRequestsDenied = 5

	snaps := s.SnapshotAndReset()
	if len(snaps) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(snaps))
	}
	snap := snaps[0]
	if snap.NumRequestsAllowed != 100 {
		t.Fatalf("snapshot allowed: got %d, want 100", snap.NumRequestsAllowed)
	}
	if snap.NumRequestsDenied != 5 {
		t.Fatalf("snapshot denied: got %d, want 5", snap.NumRequestsDenied)
	}

	// Originals should be zeroed.
	if b.NumRequestsAllowed != 0 || b.NumRequestsDenied != 0 {
		t.Fatal("counters not reset after snapshot")
	}
	if b.LastReport.IsZero() {
		t.Fatal("LastReport not updated after snapshot")
	}
}

func TestStore_SnapshotAndReset_Empty(t *testing.T) {
	s := NewMemoryBucketStore()
	snaps := s.SnapshotAndReset()
	if len(snaps) != 0 {
		t.Fatal("snapshot of empty store should be empty")
	}
}

func TestStore_SnapshotAndReset_MultipleBuckets(t *testing.T) {
	s := NewMemoryBucketStore()
	b1, _ := s.GetOrCreate(bucketId("a", "1"))
	b1.NumRequestsAllowed = 10
	b2, _ := s.GetOrCreate(bucketId("b", "2"))
	b2.NumRequestsAllowed = 20

	snaps := s.SnapshotAndReset()
	if len(snaps) != 2 {
		t.Fatalf("expected 2 snapshots, got %d", len(snaps))
	}
	var total uint64
	for _, snap := range snaps {
		total += snap.NumRequestsAllowed
	}
	if total != 30 {
		t.Fatalf("expected total 30, got %d", total)
	}
}

// --- Concurrency ---

func TestStore_ConcurrentGetOrCreate(t *testing.T) {
	s := NewMemoryBucketStore()
	id := bucketId("svc", "web")
	const goroutines = 100

	var wg sync.WaitGroup
	results := make([]*BucketState, goroutines)
	wg.Add(goroutines)
	for i := range goroutines {
		go func(idx int) {
			defer wg.Done()
			b, _ := s.GetOrCreate(id)
			results[idx] = b
		}(i)
	}
	wg.Wait()

	if s.Len() != 1 {
		t.Fatalf("expected 1 bucket, got %d", s.Len())
	}
	// All goroutines must have gotten the same pointer.
	for i := 1; i < goroutines; i++ {
		if results[i] != results[0] {
			t.Fatalf("goroutine %d got different pointer", i)
		}
	}
}

func TestStore_ConcurrentReadWrite(t *testing.T) {
	s := NewMemoryBucketStore()
	const n = 50

	var wg sync.WaitGroup
	wg.Add(n * 3) // writers + readers + deleters

	// Writers.
	for i := range n {
		go func(i int) {
			defer wg.Done()
			id := bucketId("key", string(rune('a'+i%26)))
			s.GetOrCreate(id)
		}(i)
	}

	// Readers.
	for range n {
		go func() {
			defer wg.Done()
			s.Len()
			s.ForEach(func(_ CanonicalBucketKey, _ *BucketState) {})
		}()
	}

	// Deleters.
	for i := range n {
		go func(i int) {
			defer wg.Done()
			id := bucketId("key", string(rune('a'+i%26)))
			s.Delete(id)
		}(i)
	}

	wg.Wait()
	// No panics or data races = pass. Run with -race to verify.
}
