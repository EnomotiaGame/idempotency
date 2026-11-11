package idempotency

import (
	"context"
	"time"
)

// Record is what a store keeps about one key.
type Record struct {
	// Fingerprint identifies the request that took the key (see Fingerprint).
	Fingerprint []byte
	// Status is 0 while the first request is still running, and the HTTP
	// status of its answer once it has finished.
	Status      int
	ContentType string
	Body        []byte
}

// Claim is one request asking for a key.
type Claim struct {
	// Scope is the namespace the key lives in: an account, a tenant, an API
	// key. Two scopes that happen to choose the same key never meet.
	Scope string
	Key   string
	// Fingerprint identifies the request (see Fingerprint).
	Fingerprint []byte
	// Route is the route pattern the request reached, kept for an operator
	// reading the store. It is never compared.
	Route string
	// Lock names this request. Only the request holding the lock may complete
	// or release the key.
	Lock string
	Now  time.Time
	// ExpiresAt is when the key may be used for a new request again.
	ExpiresAt time.Time
	// StaleBefore: a key still running whose lock was taken at or before this
	// moment belongs to a request that died, and may be taken over — but only
	// by a request with the same fingerprint.
	StaleBefore time.Time
}

// Store keeps the keys. It must be safe for concurrent use, and Claim must be
// atomic: of two requests claiming the same free key at once, exactly one
// gets it. The storetest package checks a store against these rules.
type Store interface {
	// Claim takes the key for this request and reports whether it got it. It
	// gets it when the key is unknown, when the key has expired, or when the
	// key is still running, was locked at or before c.StaleBefore and carries
	// the same fingerprint. Taking a key resets it: no status, no answer, the
	// new lock and the new expiry.
	Claim(ctx context.Context, c Claim) (ok bool, err error)
	// Get reads a key. found is false when there is none.
	Get(ctx context.Context, scope, key string) (rec Record, found bool, err error)
	// Complete writes the answer, only if lock still holds the key and the key
	// has no answer yet. It reports whether it wrote it.
	Complete(ctx context.Context, scope, key, lock string, rec Record, now time.Time) (ok bool, err error)
	// Release forgets the key, only if lock still holds it and it has no answer.
	Release(ctx context.Context, scope, key, lock string) error
}
