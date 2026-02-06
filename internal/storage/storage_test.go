package storage

import "testing"

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
