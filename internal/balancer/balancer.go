// Package balancer orders the credentials of one provider and remembers which
// ones recently failed.
//
// It knows nothing about HTTP: a key is an opaque connection id, a failure is a
// cooldown duration. That keeps the policy (rotate, stick to a session, back
// off) separate from the transport and easy to test with a fake clock.
package balancer

import (
	"hash/fnv"
	"sync"
	"time"
)

// Balancer is safe for concurrent use: one snapshot of the config serves many
// requests and each picks its own connection order.
type Balancer struct {
	mu       sync.Mutex
	cursor   map[string]uint64
	cooldown map[string]time.Time
	failures map[string]int
	now      func() time.Time
}

// New returns a balancer using the wall clock.
func New() *Balancer {
	return &Balancer{
		cursor:   make(map[string]uint64),
		cooldown: make(map[string]time.Time),
		failures: make(map[string]int),
		now:      time.Now,
	}
}

// NewWithClock is for tests that need deterministic backoff.
func NewWithClock(now func() time.Time) *Balancer {
	b := New()
	if now != nil {
		b.now = now
	}
	return b
}

// Order returns the indexes of keys in the order they should be tried, plus
// whether at least one key is outside its cooldown.
//
// Keys in cooldown are left out. If every key is cooling down, all of them are
// returned anyway and healthy is false: the caller can either try them as a
// last resort or move on to another combo member.
//
// A session key makes the choice sticky, which keeps an upstream prompt cache
// warm. Without one, the starting index advances per request.
func (b *Balancer) Order(providerID string, keys []string, session string) ([]int, bool) {
	if len(keys) == 0 {
		return nil, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	eligible := make([]int, 0, len(keys))
	for i, key := range keys {
		until, cooling := b.cooldown[key]
		if !cooling || !now.Before(until) {
			eligible = append(eligible, i)
		}
	}
	healthy := len(eligible) > 0
	if !healthy {
		eligible = make([]int, len(keys))
		for i := range keys {
			eligible[i] = i
		}
	}

	n := len(eligible)
	start := 0
	if session != "" {
		start = int(hashKey(providerID, session) % uint64(n))
	} else {
		start = int(b.cursor[providerID] % uint64(n))
		b.cursor[providerID]++
	}

	out := make([]int, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, eligible[(start+i)%n])
	}
	return out, healthy
}

// Fail records a failure and returns the cooldown it applied.
//
// base is the first backoff; repeated failures double it up to max. A base of
// zero means "do not cool this key down" for a request-scoped error such as a
// bad parameter: the account is fine, this request was not.
func (b *Balancer) Fail(key string, base, max time.Duration) time.Duration {
	if key == "" || base <= 0 {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	b.failures[key]++
	d := base
	for i := 1; i < b.failures[key] && d < max; i++ {
		d *= 2
	}
	if max > 0 && d > max {
		d = max
	}
	b.cooldown[key] = b.now().Add(d)
	return d
}

// Succeed clears the failure history of a key.
func (b *Balancer) Succeed(key string) {
	if key == "" {
		return
	}
	b.mu.Lock()
	delete(b.failures, key)
	delete(b.cooldown, key)
	b.mu.Unlock()
}

// Cooling reports whether a key is in a cooldown right now. Exposed for tests
// and diagnostics.
func (b *Balancer) Cooling(key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	until, ok := b.cooldown[key]
	return ok && b.now().Before(until)
}

func hashKey(providerID, session string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(providerID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(session))
	return h.Sum64()
}
