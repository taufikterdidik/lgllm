package server

import (
	"sync"
	"time"
)

// comboStatusStore keeps runtime-only combo state for the admin UI: which
// member last served a request and the last error seen for each member. It is
// deliberately in memory and keyed by the combo name, because a reload swaps the
// registry snapshot but should not erase the recent diagnostic history.
type comboStatusStore struct {
	mu     sync.Mutex
	combos map[string]*comboStatus
}

type comboStatus struct {
	lastMember string
	lastUsed   time.Time
	errors     map[string]comboMemberError
}

// comboMemberError is the last failure attributed to one configured member.
type comboMemberError struct {
	Message    string    `json:"message"`
	Status     int       `json:"status,omitempty"`
	Connection string    `json:"connection,omitempty"`
	At         time.Time `json:"at"`
}

func newComboStatusStore() *comboStatusStore {
	return &comboStatusStore{combos: make(map[string]*comboStatus)}
}

// Used records the member that just served a request. The member's previous
// error is cleared: a successful answer is stronger evidence than an old
// failure, and keeps the highlighted row from also looking broken.
func (s *comboStatusStore) Used(combo, member string, at time.Time) {
	if s == nil || combo == "" || member == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.combos[combo]
	if st == nil {
		st = &comboStatus{errors: make(map[string]comboMemberError)}
		s.combos[combo] = st
	}
	st.lastMember = member
	st.lastUsed = at
	delete(st.errors, member)
}

// Failed records one failed attempt for a member. A newer error wins; an
// out-of-order completion from a concurrent request cannot replace it.
func (s *comboStatusStore) Failed(combo, member string, status int, connection, message string, at time.Time) {
	if s == nil || combo == "" || member == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.combos[combo]
	if st == nil {
		st = &comboStatus{errors: make(map[string]comboMemberError)}
		s.combos[combo] = st
	}
	if prev, ok := st.errors[member]; ok && prev.At.After(at) {
		return
	}
	st.errors[member] = comboMemberError{
		Message:    message,
		Status:     status,
		Connection: connection,
		At:         at,
	}
}

// Snapshot copies one combo's state for a response. The map is never shared with
// callers, so encoding cannot race a concurrent request.
func (s *comboStatusStore) Snapshot(combo string) (lastMember string, lastUsed time.Time, errors map[string]comboMemberError) {
	if s == nil || combo == "" {
		return "", time.Time{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.combos[combo]
	if st == nil {
		return "", time.Time{}, nil
	}
	if len(st.errors) > 0 {
		errors = make(map[string]comboMemberError, len(st.errors))
		for member, err := range st.errors {
			errors[member] = err
		}
	}
	return st.lastMember, st.lastUsed, errors
}

// recordComboFailure attributes the last failure of one member attempt to the
// Combo tab. Non-combo requests have an empty member and are ignored.
func (s *Server) recordComboFailure(combo, member string, failures []attemptFailure) {
	if s.comboStatus == nil || combo == "" || member == "" || len(failures) == 0 {
		return
	}
	last := failures[len(failures)-1]
	s.comboStatus.Failed(combo, member, last.Status, last.Connection, last.Error, time.Now())
}
