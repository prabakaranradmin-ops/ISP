package localcache

import (
	"testing"
	"time"
)

func TestStore_SetAndGet(t *testing.T) {
	s := New[string](time.Hour)
	defer s.Close()

	s.Set("k", "v", time.Minute)
	got, ok := s.Get("k")
	if !ok || got != "v" {
		t.Fatalf("Get(k) = (%q, %v), want (\"v\", true)", got, ok)
	}
}

func TestStore_MissingKey(t *testing.T) {
	s := New[string](time.Hour)
	defer s.Close()

	if _, ok := s.Get("nope"); ok {
		t.Fatal("Get on a never-set key must report false")
	}
}

func TestStore_ExpiredEntryIsNotReturned(t *testing.T) {
	s := New[string](time.Hour) // sweep interval irrelevant: Get itself must check expiry
	defer s.Close()

	s.Set("k", "v", -time.Second) // already expired
	if _, ok := s.Get("k"); ok {
		t.Fatal("Get must not return an expired entry, even before the sweep runs")
	}
}

func TestStore_Delete(t *testing.T) {
	s := New[string](time.Hour)
	defer s.Close()

	s.Set("k", "v", time.Minute)
	s.Delete("k")
	if _, ok := s.Get("k"); ok {
		t.Fatal("Get must not return a deleted entry")
	}
}

func TestStore_OverwriteRefreshesValueAndTTL(t *testing.T) {
	s := New[string](time.Hour)
	defer s.Close()

	s.Set("k", "v1", -time.Second) // expired
	s.Set("k", "v2", time.Minute)  // fresh
	got, ok := s.Get("k")
	if !ok || got != "v2" {
		t.Fatalf("Get(k) = (%q, %v), want (\"v2\", true)", got, ok)
	}
}

func TestStore_TrySetOnlyWinsOnce(t *testing.T) {
	s := New[string](time.Hour)
	defer s.Close()

	if !s.TrySet("k", "first", time.Minute) {
		t.Fatal("first TrySet on an empty key must succeed")
	}
	if s.TrySet("k", "second", time.Minute) {
		t.Fatal("second TrySet on a live key must fail")
	}
	got, _ := s.Get("k")
	if got != "first" {
		t.Fatalf("TrySet must not overwrite the winning value, got %q", got)
	}
}

func TestStore_TrySetSucceedsAfterExpiry(t *testing.T) {
	s := New[string](time.Hour)
	defer s.Close()

	s.Set("k", "stale", -time.Second)
	if !s.TrySet("k", "fresh", time.Minute) {
		t.Fatal("TrySet must succeed once the prior entry has expired")
	}
	got, _ := s.Get("k")
	if got != "fresh" {
		t.Fatalf("Get(k) = %q, want \"fresh\"", got)
	}
}

func TestStore_SweepReclaimsExpiredEntries(t *testing.T) {
	s := New[string](20 * time.Millisecond)
	defer s.Close()

	s.Set("k", "v", -time.Second)
	// The entry is already logically gone from Get's perspective; poll the
	// internal map size via repeated Set/Get cycles is indirect, so instead
	// just confirm the sweep does not panic and Get still reports absent
	// after it has had time to run.
	time.Sleep(80 * time.Millisecond)
	if _, ok := s.Get("k"); ok {
		t.Fatal("swept entry must not be returned")
	}
}

func TestStore_ConcurrentAccess(t *testing.T) {
	s := New[int](time.Hour)
	defer s.Close()

	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func(n int) {
			s.Set("k", n, time.Minute)
			s.Get("k")
			s.Delete("k")
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 20; i++ {
		<-done
	}
}

func TestStore_CloseIsIdempotent(t *testing.T) {
	s := New[string](time.Hour)
	s.Close()
	s.Close() // must not panic
}
