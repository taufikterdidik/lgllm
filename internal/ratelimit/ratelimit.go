// Package ratelimit applies per-client request limits and a daily token quota.
//
// The state is in memory and keyed by an opaque client identity (a hash of the
// API key, or "local" for a trusted loopback client). Requests use a token
// bucket so a short burst is allowed; tokens are a counter that resets at UTC
// midnight. Both are optional.
package ratelimit

import (
	"sync"
	"time"
)

// Config is the current limit policy. It is fetched per request so a config
// reload applies without rebuilding the limiter.
type Config struct {
	RequestsPerMinute int
	Burst             int
	TokensPerDay      int
}

// Decision is the result of Allow.
type Decision struct {
	Allowed    bool
	RetryAfter time.Duration
	// Reason is "rate" or "quota" when denied.
	Reason string
}

type bucket struct {
	tokens float64
	last   time.Time
}

type quota struct {
	day  string
	used int
}

// Limiter is safe for concurrent use.
type Limiter struct {
	mu      sync.Mutex
	cfg     func() Config
	now     func() time.Time
	buckets map[string]*bucket
	quotas  map[string]*quota
}

// New returns a limiter reading its policy from cfg.
func New(cfg func() Config) *Limiter {
	return NewWithClock(cfg, time.Now)
}

// NewWithClock is for tests that need deterministic refills.
func NewWithClock(cfg func() Config, now func() time.Time) *Limiter {
	if cfg == nil {
		cfg = func() Config { return Config{} }
	}
	if now == nil {
		now = time.Now
	}
	return &Limiter{
		cfg:     cfg,
		now:     now,
		buckets: make(map[string]*bucket),
		quotas:  make(map[string]*quota),
	}
}

// Allow consumes one request from key's bucket.
func (l *Limiter) Allow(key string) Decision {
	if l == nil {
		return Decision{Allowed: true}
	}
	cfg := l.cfg()
	if cfg.RequestsPerMinute <= 0 && cfg.TokensPerDay <= 0 {
		return Decision{Allowed: true}
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()

	if cfg.TokensPerDay > 0 {
		q := l.quotaFor(key, now)
		if q.used >= cfg.TokensPerDay {
			return Decision{Allowed: false, Reason: "quota", RetryAfter: untilMidnight(now)}
		}
	}

	if cfg.RequestsPerMinute > 0 {
		burst := cfg.Burst
		if burst <= 0 {
			burst = cfg.RequestsPerMinute
		}
		b := l.buckets[key]
		if b == nil {
			b = &bucket{tokens: float64(burst), last: now}
			l.buckets[key] = b
		}
		ratePerSecond := float64(cfg.RequestsPerMinute) / 60
		b.tokens += now.Sub(b.last).Seconds() * ratePerSecond
		if b.tokens > float64(burst) {
			b.tokens = float64(burst)
		}
		b.last = now
		if b.tokens < 1 {
			missing := 1 - b.tokens
			return Decision{
				Allowed: false, Reason: "rate",
				RetryAfter: time.Duration(missing / ratePerSecond * float64(time.Second)),
			}
		}
		b.tokens--
	}
	return Decision{Allowed: true}
}

// AddTokens charges tokens to the daily quota of key.
func (l *Limiter) AddTokens(key string, n int) {
	if l == nil || n <= 0 {
		return
	}
	cfg := l.cfg()
	if cfg.TokensPerDay <= 0 {
		return
	}
	l.mu.Lock()
	q := l.quotaFor(key, l.now())
	q.used += n
	l.mu.Unlock()
}

// Used reports how many tokens key has spent today. Exposed for /usage and tests.
func (l *Limiter) Used(key string) int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	q := l.quotaFor(key, l.now())
	return q.used
}

func (l *Limiter) quotaFor(key string, now time.Time) *quota {
	day := now.UTC().Format("2006-01-02")
	q := l.quotas[key]
	if q == nil || q.day != day {
		q = &quota{day: day}
		l.quotas[key] = q
	}
	return q
}

func untilMidnight(now time.Time) time.Duration {
	utc := now.UTC()
	next := time.Date(utc.Year(), utc.Month(), utc.Day()+1, 0, 0, 0, 0, time.UTC)
	d := next.Sub(utc)
	if d <= 0 {
		return 24 * time.Hour
	}
	return d
}
