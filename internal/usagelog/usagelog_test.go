package usagelog

import (
	"testing"
	"time"
)

func TestRingWrapsAndReturnsNewestFirst(t *testing.T) {
	s := New(3)
	for i := 0; i < 5; i++ {
		s.Add(Entry{Status: 200 + i, Time: time.Unix(int64(i), 0)})
	}
	if got := s.Len(); got != 3 {
		t.Fatalf("len = %d", got)
	}
	entries := s.Snapshot(10)
	if len(entries) != 3 {
		t.Fatalf("snapshot = %d entries", len(entries))
	}
	for i, want := range []int{204, 203, 202} {
		if entries[i].Status != want {
			t.Errorf("entry %d = %d, want %d", i, entries[i].Status, want)
		}
	}
}

func TestSnapshotLimit(t *testing.T) {
	s := New(10)
	for i := 0; i < 4; i++ {
		s.Add(Entry{Status: i})
	}
	got := s.Snapshot(2)
	if len(got) != 2 || got[0].Status != 3 || got[1].Status != 2 {
		t.Errorf("snapshot = %+v", got)
	}
	if len(s.Snapshot(0)) != 4 {
		t.Error("limit 0 should return everything")
	}
}

func TestNilStoreIsSafe(t *testing.T) {
	s := New(0)
	if s != nil {
		t.Fatal("capacity 0 should disable the store")
	}
	s.Add(Entry{})
	if s.Len() != 0 || s.Snapshot(5) != nil {
		t.Error("nil store should be a no-op")
	}
}

func TestSubscribeReceivesAndCancels(t *testing.T) {
	s := New(3)
	ch, cancel := s.Subscribe()
	s.Add(Entry{Status: 200})

	select {
	case e := <-ch:
		if e.Status != 200 {
			t.Errorf("entry = %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber received nothing")
	}

	cancel()
	s.Add(Entry{Status: 201}) // must not panic after cancel
	if _, ok := <-ch; ok {
		t.Error("channel was not closed on cancel")
	}
}

func TestNilSubscribeIsClosed(t *testing.T) {
	var s *Store
	ch, cancel := s.Subscribe()
	defer cancel()
	if _, ok := <-ch; ok {
		t.Error("nil store subscriber should be closed")
	}
}
