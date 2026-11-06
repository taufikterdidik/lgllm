// Package usagelog keeps the most recent requests in memory for the /usage
// endpoint and (later) the admin UI. It is a ring buffer, not a database: the
// goal is debugging the last few hundred requests, not billing.
package usagelog

import (
	"sync"
	"time"
)

// Entry is one finished request.
type Entry struct {
	Time             time.Time `json:"time"`
	RequestID        string    `json:"request_id,omitempty"`
	Ingress          string    `json:"ingress"`
	Requested        string    `json:"requested"`
	Provider         string    `json:"provider,omitempty"`
	Model            string    `json:"model,omitempty"`
	Status           int       `json:"status"`
	Stream           bool      `json:"stream"`
	DurationMS       int64     `json:"duration_ms"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	SavedBytes       int       `json:"saved_bytes"`
	CostUSD          float64   `json:"cost_usd"`
	CostKnown        bool      `json:"cost_known"`
	Error            string    `json:"error,omitempty"`
}

// Store is a fixed-size ring buffer, safe for concurrent use. A nil Store is
// valid and ignores writes, which is how "usage_log_size: 0" disables it.
type Store struct {
	mu       sync.Mutex
	capacity int
	entries  []Entry
	next     int
	full     bool
}

// New returns a store holding at most capacity entries. A non-positive capacity
// returns nil, whose methods are no-ops.
func New(capacity int) *Store {
	if capacity <= 0 {
		return nil
	}
	return &Store{capacity: capacity, entries: make([]Entry, capacity)}
}

// Add appends an entry, overwriting the oldest once the buffer is full.
func (s *Store) Add(e Entry) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.entries[s.next] = e
	s.next = (s.next + 1) % s.capacity
	if s.next == 0 {
		s.full = true
	}
	s.mu.Unlock()
}

// Len reports how many entries are currently stored.
func (s *Store) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.full {
		return s.capacity
	}
	return s.next
}

// Snapshot returns up to limit entries, newest first.
func (s *Store) Snapshot(limit int) []Entry {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	n := s.next
	if s.full {
		n = s.capacity
	}
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]Entry, 0, limit)
	for i := 0; i < limit; i++ {
		idx := (s.next - 1 - i + s.capacity*2) % s.capacity
		out = append(out, s.entries[idx])
	}
	return out
}
