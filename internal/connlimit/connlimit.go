// Package connlimit caps how many upstream attempts one credential may run at
// the same time.
//
// The limit protects the account, not the client: a provider that starts
// returning 429 under parallel streams is better served by falling through to
// another connection or combo member than by queueing every request. State is
// in memory and keyed by connection id, so a config reload can change the cap
// without losing the attempts already in flight.
package connlimit

import "sync"

// Limiter is safe for concurrent use.
type Limiter struct {
	mu     sync.Mutex
	active map[string]int
}

// New returns a limiter with no active attempts.
func New() *Limiter {
	return &Limiter{active: make(map[string]int)}
}

// TryAcquire reserves one slot for connection id when max is positive. It
// returns a release function and true on success, or a no-op release and false
// when the connection is already at its limit. A max of zero or less means
// unlimited and always succeeds.
//
// The caller must call release exactly once, after the upstream body has been
// consumed (or the attempt abandoned), so the slot covers the whole stream.
func (l *Limiter) TryAcquire(id string, max int) (func(), bool) {
	if l == nil || max <= 0 {
		return func() {}, true
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active[id] >= max {
		return func() {}, false
	}
	l.active[id]++

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			if l.active[id] <= 1 {
				delete(l.active, id)
			} else {
				l.active[id]--
			}
			l.mu.Unlock()
		})
	}, true
}

// Active reports how many attempts hold a slot for connection id.
func (l *Limiter) Active(id string) int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active[id]
}
