package balancer

import (
	"testing"
	"time"
)

func clockAt(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func TestOrderRotates(t *testing.T) {
	b := NewWithClock(clockAt(time.Unix(0, 0)))
	keys := []string{"a", "b", "c"}

	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		order, _ := b.Order("p", keys, "")
		if len(order) != 3 {
			t.Fatalf("order = %v", order)
		}
		seen[keys[order[0]]]++
	}
	for _, k := range keys {
		if seen[k] != 2 {
			t.Errorf("start counts = %v, want each key twice", seen)
		}
	}
}

func TestOrderSkipsCooldown(t *testing.T) {
	now := time.Unix(0, 0)
	b := NewWithClock(func() time.Time { return now })
	keys := []string{"a", "b", "c"}

	b.Fail("b", time.Minute, time.Hour)
	for i := 0; i < 5; i++ {
		order, _ := b.Order("p", keys, "")
		for _, idx := range order {
			if keys[idx] == "b" {
				t.Fatal("a cooling key was offered")
			}
		}
	}
	if !b.Cooling("b") {
		t.Fatal("b should be cooling")
	}
}

func TestOrderFallsBackWhenAllCooling(t *testing.T) {
	now := time.Unix(0, 0)
	b := NewWithClock(func() time.Time { return now })
	keys := []string{"a", "b"}
	b.Fail("a", time.Minute, time.Hour)
	b.Fail("b", time.Minute, time.Hour)

	if got, healthy := b.Order("p", keys, ""); len(got) != 2 || healthy {
		t.Fatalf("order = %v, want both keys as a last resort", got)
	}
}

func TestStickySession(t *testing.T) {
	b := NewWithClock(clockAt(time.Unix(0, 0)))
	keys := []string{"a", "b", "c", "d"}

	order, _ := b.Order("p", keys, "session-1")
	first := keys[order[0]]
	for i := 0; i < 5; i++ {
		order, _ := b.Order("p", keys, "session-1")
		got := keys[order[0]]
		if got != first {
			t.Fatalf("session moved from %s to %s", first, got)
		}
	}
}

func TestBackoffDoublesAndCaps(t *testing.T) {
	now := time.Unix(0, 0)
	b := NewWithClock(func() time.Time { return now })

	if d := b.Fail("k", time.Second, 4*time.Second); d != time.Second {
		t.Errorf("first cooldown = %v", d)
	}
	if d := b.Fail("k", time.Second, 4*time.Second); d != 2*time.Second {
		t.Errorf("second cooldown = %v", d)
	}
	if d := b.Fail("k", time.Second, 4*time.Second); d != 4*time.Second {
		t.Errorf("third cooldown = %v", d)
	}
	if d := b.Fail("k", time.Second, 4*time.Second); d != 4*time.Second {
		t.Errorf("cooldown exceeded the cap: %v", d)
	}

	now = now.Add(4 * time.Second)
	if b.Cooling("k") {
		t.Error("cooldown should have expired")
	}
	b.Succeed("k")
	if d := b.Fail("k", time.Second, 4*time.Second); d != time.Second {
		t.Errorf("success did not reset the backoff: %v", d)
	}
}

func TestRequestScopedFailureDoesNotCool(t *testing.T) {
	b := NewWithClock(clockAt(time.Unix(0, 0)))
	if d := b.Fail("k", 0, time.Hour); d != 0 {
		t.Errorf("cooldown = %v, want none", d)
	}
	if b.Cooling("k") {
		t.Error("a request-scoped failure must not cool the key")
	}
}

func TestOrderEmpty(t *testing.T) {
	if got, healthy := New().Order("p", nil, ""); got != nil || healthy {
		t.Errorf("order = %v, healthy = %v", got, healthy)
	}
}
