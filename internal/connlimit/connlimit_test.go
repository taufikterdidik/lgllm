package connlimit

import "testing"

func TestTryAcquireEnforcesLimit(t *testing.T) {
	l := New()
	release1, ok := l.TryAcquire("c", 2)
	if !ok {
		t.Fatal("first acquire failed")
	}
	release2, ok := l.TryAcquire("c", 2)
	if !ok {
		t.Fatal("second acquire failed")
	}
	if _, ok := l.TryAcquire("c", 2); ok {
		t.Fatal("third acquire succeeded past the limit")
	}
	if got := l.Active("c"); got != 2 {
		t.Errorf("active = %d, want 2", got)
	}

	release1()
	if got := l.Active("c"); got != 1 {
		t.Errorf("active after release = %d, want 1", got)
	}
	release3, ok := l.TryAcquire("c", 2)
	if !ok {
		t.Fatal("acquire after release failed")
	}
	release2()
	release3()
	if got := l.Active("c"); got != 0 {
		t.Errorf("active after all releases = %d, want 0", got)
	}
}

func TestUnlimitedAndNil(t *testing.T) {
	l := New()
	for i := 0; i < 5; i++ {
		release, ok := l.TryAcquire("c", 0)
		if !ok {
			t.Fatal("unlimited acquire failed")
		}
		release()
	}

	var nilLimiter *Limiter
	if _, ok := nilLimiter.TryAcquire("c", 1); !ok {
		t.Fatal("nil limiter rejected a request")
	}
	if got := nilLimiter.Active("c"); got != 0 {
		t.Errorf("nil active = %d", got)
	}
}

func TestReleaseIsIdempotentAndIndependentPerConnection(t *testing.T) {
	l := New()
	release, ok := l.TryAcquire("a", 1)
	if !ok {
		t.Fatal("acquire a failed")
	}
	if _, ok := l.TryAcquire("b", 1); !ok {
		t.Fatal("connection b should have its own slot")
	}
	release()
	release()
	if got := l.Active("a"); got != 0 {
		t.Errorf("active a = %d, want 0 after double release", got)
	}
}
