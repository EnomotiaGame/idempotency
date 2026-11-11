// Package memstore keeps idempotency keys in memory.
//
// It suits tests and a server that runs as a single process. Keys do not
// survive a restart and are not shared between instances: behind a load
// balancer use a shared store such as pgstore.
package memstore

import (
	"bytes"
	"context"
	"sync"
	"time"

	"github.com/EnomotiaGame/idempotency"
)

// Store is an in-memory idempotency.Store. The zero value is not usable; call
// New.
type Store struct {
	mu   sync.Mutex
	rows map[id]*row
}

type id struct{ scope, key string }

type row struct {
	rec     idempotency.Record
	lock    string
	locked  time.Time
	expires time.Time
}

// New returns an empty store.
func New() *Store { return &Store{rows: map[id]*row{}} }

// Claim implements idempotency.Store.
func (s *Store) Claim(_ context.Context, c idempotency.Claim) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := id{c.Scope, c.Key}
	if r, ok := s.rows[k]; ok {
		expired := !r.expires.After(c.Now)
		stale := r.rec.Status == 0 && !r.locked.After(c.StaleBefore) &&
			bytes.Equal(r.rec.Fingerprint, c.Fingerprint)
		if !expired && !stale {
			return false, nil
		}
	}
	s.rows[k] = &row{
		rec:     idempotency.Record{Fingerprint: bytes.Clone(c.Fingerprint)},
		lock:    c.Lock,
		locked:  c.Now,
		expires: c.ExpiresAt,
	}
	return true, nil
}

// Get implements idempotency.Store.
func (s *Store) Get(_ context.Context, scope, key string) (idempotency.Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id{scope, key}]
	if !ok {
		return idempotency.Record{}, false, nil
	}
	rec := r.rec
	rec.Fingerprint = bytes.Clone(rec.Fingerprint)
	rec.Body = bytes.Clone(rec.Body)
	return rec, true, nil
}

// Complete implements idempotency.Store.
func (s *Store) Complete(_ context.Context, scope, key, lock string, rec idempotency.Record, _ time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id{scope, key}]
	if !ok || r.lock != lock || r.rec.Status != 0 {
		return false, nil
	}
	r.rec = idempotency.Record{
		Fingerprint: r.rec.Fingerprint,
		Status:      rec.Status,
		ContentType: rec.ContentType,
		Body:        bytes.Clone(rec.Body),
	}
	return true, nil
}

// Release implements idempotency.Store.
func (s *Store) Release(_ context.Context, scope, key, lock string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := id{scope, key}
	if r, ok := s.rows[k]; ok && r.lock == lock && r.rec.Status == 0 {
		delete(s.rows, k)
	}
	return nil
}

// Sweep deletes the keys that expired at or before now, and reports how many.
// Call it now and then, or memory grows with every key ever used.
func (s *Store) Sweep(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, r := range s.rows {
		if !r.expires.After(now) {
			delete(s.rows, k)
			n++
		}
	}
	return n
}

// Len is the number of keys held.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rows)
}

var _ idempotency.Store = (*Store)(nil)
