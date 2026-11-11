// Package storetest checks an idempotency.Store against the rules the
// middleware relies on. Run it from your store's own test:
//
//	func TestMyStore(t *testing.T) {
//		storetest.Run(t, func(t *testing.T) idempotency.Store { return newMyStore(t) })
//	}
//
// The factory is called once per sub-test and must return an empty store, or
// one where the scopes the suite uses (each sub-test makes its own) are unused.
package storetest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"github.com/EnomotiaGame/idempotency"
)

// Run runs the suite.
func Run(t *testing.T, newStore func(t *testing.T) idempotency.Store) {
	t.Helper()
	for _, c := range []struct {
		name string
		fn   func(t *testing.T, s idempotency.Store, h *helper)
	}{
		{"a free key is claimed once", freeKeyClaimedOnce},
		{"get reads a running key, then its answer", getReadsRunningThenAnswer},
		{"only the holder completes", onlyHolderCompletes},
		{"an answer is written once", answerWrittenOnce},
		{"only the holder releases, and only while running", onlyHolderReleases},
		{"an expired key is taken by any request", expiredKeyTaken},
		{"a stale key is taken only by the same request", staleKeyTakenBySameRequest},
		{"a finished key is never taken while it lives", finishedKeyNeverTaken},
		{"scopes do not meet", scopesDoNotMeet},
		{"of many claims at once exactly one wins", concurrentClaims},
		{"an empty body is kept as empty", emptyBodyKept},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newStore(t)
			c.fn(t, s, newHelper(t, s))
		})
	}
}

var base = time.Date(2100, 1, 1, 12, 0, 0, 0, time.UTC)

const (
	retention  = 24 * time.Hour
	staleAfter = 2 * time.Minute
)

type helper struct {
	t     *testing.T
	s     idempotency.Store
	ctx   context.Context
	scope string
	key   string
	fp    []byte
	other []byte
}

func newHelper(t *testing.T, s idempotency.Store) *helper {
	return &helper{
		t: t, s: s, ctx: context.Background(),
		scope: "scope-" + random(), key: "key-" + random(),
		fp:    idempotency.Fingerprint("POST", "/orders", "", []byte(`{"amount":1}`)),
		other: idempotency.Fingerprint("POST", "/orders", "", []byte(`{"amount":2}`)),
	}
}

func random() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (h *helper) claim(fp []byte, at time.Time) (string, bool) {
	h.t.Helper()
	return h.claimIn(h.scope, fp, at)
}

func (h *helper) claimIn(scope string, fp []byte, at time.Time) (string, bool) {
	h.t.Helper()
	lock := random()
	ok, err := h.s.Claim(h.ctx, idempotency.Claim{
		Scope: scope, Key: h.key, Fingerprint: fp, Route: "POST /orders", Lock: lock,
		Now: at, ExpiresAt: at.Add(retention), StaleBefore: at.Add(-staleAfter),
	})
	if err != nil {
		h.t.Fatalf("claim: %v", err)
	}
	return lock, ok
}

func (h *helper) get() (idempotency.Record, bool) {
	h.t.Helper()
	rec, found, err := h.s.Get(h.ctx, h.scope, h.key)
	if err != nil {
		h.t.Fatalf("get: %v", err)
	}
	return rec, found
}

func (h *helper) complete(lock string, status int, body string, at time.Time) bool {
	h.t.Helper()
	ok, err := h.s.Complete(h.ctx, h.scope, h.key, lock, idempotency.Record{
		Fingerprint: h.fp, Status: status, ContentType: "application/json", Body: []byte(body),
	}, at)
	if err != nil {
		h.t.Fatalf("complete: %v", err)
	}
	return ok
}

func (h *helper) release(lock string) {
	h.t.Helper()
	if err := h.s.Release(h.ctx, h.scope, h.key, lock); err != nil {
		h.t.Fatalf("release: %v", err)
	}
}

func freeKeyClaimedOnce(t *testing.T, _ idempotency.Store, h *helper) {
	if _, ok := h.claim(h.fp, base); !ok {
		t.Fatal("a free key was not claimed")
	}
	if _, ok := h.claim(h.fp, base); ok {
		t.Fatal("a held key was claimed again by the same request")
	}
	if _, ok := h.claim(h.other, base); ok {
		t.Fatal("a held key was claimed by another request")
	}
}

func getReadsRunningThenAnswer(t *testing.T, _ idempotency.Store, h *helper) {
	if _, found := h.get(); found {
		t.Fatal("an unknown key was found")
	}
	lock, _ := h.claim(h.fp, base)
	rec, found := h.get()
	if !found || rec.Status != 0 || !bytes.Equal(rec.Fingerprint, h.fp) {
		t.Fatalf("a running key reads %+v, found %v", rec, found)
	}
	if !h.complete(lock, 201, `{"id":1}`, base) {
		t.Fatal("the holder could not complete")
	}
	rec, found = h.get()
	if !found || rec.Status != 201 || string(rec.Body) != `{"id":1}` ||
		rec.ContentType != "application/json" || !bytes.Equal(rec.Fingerprint, h.fp) {
		t.Fatalf("a finished key reads %+v, found %v", rec, found)
	}
}

func onlyHolderCompletes(t *testing.T, _ idempotency.Store, h *helper) {
	h.claim(h.fp, base)
	if h.complete(random(), 200, `{}`, base) {
		t.Fatal("a stranger's lock completed the key")
	}
	if rec, _ := h.get(); rec.Status != 0 {
		t.Fatalf("the key reads %+v after a stranger's completion", rec)
	}
}

func answerWrittenOnce(t *testing.T, _ idempotency.Store, h *helper) {
	lock, _ := h.claim(h.fp, base)
	h.complete(lock, 201, `first`, base)
	if h.complete(lock, 500, `second`, base) {
		t.Fatal("a finished key was completed again")
	}
	if rec, _ := h.get(); rec.Status != 201 || string(rec.Body) != "first" {
		t.Fatalf("the answer was overwritten: %+v", rec)
	}
}

func onlyHolderReleases(t *testing.T, _ idempotency.Store, h *helper) {
	lock, _ := h.claim(h.fp, base)
	h.release(random())
	if _, found := h.get(); !found {
		t.Fatal("a stranger's lock released the key")
	}
	h.release(lock)
	if _, found := h.get(); found {
		t.Fatal("the holder could not release the key")
	}
	lock, _ = h.claim(h.fp, base)
	h.complete(lock, 200, `{}`, base)
	h.release(lock)
	if _, found := h.get(); !found {
		t.Fatal("a finished key was released")
	}
}

func expiredKeyTaken(t *testing.T, _ idempotency.Store, h *helper) {
	lock, _ := h.claim(h.fp, base)
	h.complete(lock, 200, `old`, base)
	if _, ok := h.claim(h.other, base.Add(retention-time.Second)); ok {
		t.Fatal("a living key was taken")
	}
	newLock, ok := h.claim(h.other, base.Add(retention))
	if !ok {
		t.Fatal("an expired key was not taken")
	}
	rec, _ := h.get()
	if rec.Status != 0 || len(rec.Body) != 0 || !bytes.Equal(rec.Fingerprint, h.other) {
		t.Fatalf("a taken key was not reset: %+v", rec)
	}
	if h.complete(lock, 200, `late`, base.Add(retention)) {
		t.Fatal("the old holder completed a key taken from it")
	}
	if !h.complete(newLock, 202, `new`, base.Add(retention)) {
		t.Fatal("the new holder could not complete")
	}
}

func staleKeyTakenBySameRequest(t *testing.T, _ idempotency.Store, h *helper) {
	dead, _ := h.claim(h.fp, base)
	if _, ok := h.claim(h.fp, base.Add(staleAfter-time.Second)); ok {
		t.Fatal("a fresh holder was taken over")
	}
	if _, ok := h.claim(h.other, base.Add(staleAfter)); ok {
		t.Fatal("a stale holder was taken over by another request")
	}
	alive, ok := h.claim(h.fp, base.Add(staleAfter))
	if !ok {
		t.Fatal("a stale holder was not taken over by the same request")
	}
	if h.complete(dead, 200, `dead`, base.Add(staleAfter)) {
		t.Fatal("the dead holder completed after the takeover")
	}
	if !h.complete(alive, 201, `alive`, base.Add(staleAfter)) {
		t.Fatal("the new holder could not complete")
	}
}

func finishedKeyNeverTaken(t *testing.T, _ idempotency.Store, h *helper) {
	lock, _ := h.claim(h.fp, base)
	h.complete(lock, 200, `{}`, base)
	if _, ok := h.claim(h.fp, base.Add(time.Hour)); ok {
		t.Fatal("a finished key was taken by the same request before it expired")
	}
}

func scopesDoNotMeet(t *testing.T, _ idempotency.Store, h *helper) {
	lock, _ := h.claim(h.fp, base)
	h.complete(lock, 200, `mine`, base)
	if _, ok := h.claimIn(h.scope+"-other", h.fp, base); !ok {
		t.Fatal("another scope could not use the same key")
	}
	if rec, _ := h.get(); string(rec.Body) != "mine" {
		t.Fatalf("another scope's claim changed this one: %+v", rec)
	}
}

func concurrentClaims(t *testing.T, _ idempotency.Store, h *helper) {
	const n = 16
	var wg sync.WaitGroup
	wins := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := h.s.Claim(h.ctx, idempotency.Claim{
				Scope: h.scope, Key: h.key, Fingerprint: h.fp, Route: "POST /orders", Lock: random(),
				Now: base, ExpiresAt: base.Add(retention), StaleBefore: base.Add(-staleAfter),
			})
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if ok {
				wins <- struct{}{}
			}
		}()
	}
	wg.Wait()
	close(wins)
	if got := len(wins); got != 1 {
		t.Fatalf("%d of %d concurrent claims won", got, n)
	}
}

func emptyBodyKept(t *testing.T, _ idempotency.Store, h *helper) {
	lock, _ := h.claim(h.fp, base)
	if _, err := h.s.Complete(h.ctx, h.scope, h.key, lock, idempotency.Record{Fingerprint: h.fp, Status: 204}, base); err != nil {
		t.Fatalf("complete with no body: %v", err)
	}
	if rec, found := h.get(); !found || rec.Status != 204 || len(rec.Body) != 0 {
		t.Fatalf("an answer with no body reads %+v, found %v", rec, found)
	}
}
