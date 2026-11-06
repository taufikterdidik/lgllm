package ratelimit

import (
	"testing"
	"time"
)

func fixed(now *time.Time) func() time.Time {
	return func() time.Time { return *now }
}

func TestDisabledAllowsEverything(t *testing.T) {
	l := New(func() Config { return Config{} })
	for i := 0; i < 100; i++ {
		if d := l.Allow("k"); !d.Allowed {
			t.Fatalf("request %d denied: %+v", i, d)
		}
	}
}

func TestTokenBucketBurstAndRefill(t *testing.T) {
	now := time.Unix(0, 0)
	l := NewWithClock(func() Config {
		return Config{RequestsPerMinute: 60, Burst: 2} // 1 per second
	}, fixed(&now))

	if d := l.Allow("k"); !d.Allowed {
		t.Fatal("first request denied")
	}
	if d := l.Allow("k"); !d.Allowed {
		t.Fatal("second request denied")
	}
	d := l.Allow("k")
	if d.Allowed {
		t.Fatal("burst was exceeded")
	}
	if d.Reason != "rate" || d.RetryAfter <= 0 {
		t.Errorf("decision = %+v", d)
	}

	now = now.Add(time.Second)
	if d := l.Allow("k"); !d.Allowed {
		t.Errorf("bucket did not refill: %+v", d)
	}
}

func TestBucketsArePerKey(t *testing.T) {
	now := time.Unix(0, 0)
	l := NewWithClock(func() Config { return Config{RequestsPerMinute: 60, Burst: 1} }, fixed(&now))
	if d := l.Allow("a"); !d.Allowed {
		t.Fatal("a denied")
	}
	if d := l.Allow("a"); d.Allowed {
		t.Fatal("a should be limited")
	}
	if d := l.Allow("b"); !d.Allowed {
		t.Error("b must have its own bucket")
	}
}

func TestDailyQuota(t *testing.T) {
	now := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	l := NewWithClock(func() Config {
		return Config{RequestsPerMinute: 6000, Burst: 6000, TokensPerDay: 100}
	}, fixed(&now))

	if d := l.Allow("k"); !d.Allowed {
		t.Fatal("first request denied")
	}
	l.AddTokens("k", 60)
	if d := l.Allow("k"); !d.Allowed {
		t.Fatal("below-quota request denied")
	}
	l.AddTokens("k", 50) // 110 total
	d := l.Allow("k")
	if d.Allowed {
		t.Fatal("quota should be exhausted")
	}
	if d.Reason != "quota" || d.RetryAfter <= 0 {
		t.Errorf("decision = %+v", d)
	}
	if got := l.Used("k"); got != 110 {
		t.Errorf("used = %d", got)
	}

	// The next UTC day starts fresh.
	now = time.Date(2026, 11, 8, 0, 0, 1, 0, time.UTC)
	if d := l.Allow("k"); !d.Allowed {
		t.Errorf("quota did not reset at midnight: %+v", d)
	}
	if got := l.Used("k"); got != 0 {
		t.Errorf("used after reset = %d", got)
	}
}

func TestNilLimiterAllows(t *testing.T) {
	var l *Limiter
	if d := l.Allow("k"); !d.Allowed {
		t.Error("nil limiter should allow")
	}
	l.AddTokens("k", 10) // must not panic
}
