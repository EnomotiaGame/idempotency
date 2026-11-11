package memstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/EnomotiaGame/idempotency"
	"github.com/EnomotiaGame/idempotency/memstore"
	"github.com/EnomotiaGame/idempotency/storetest"
)

func TestStore(t *testing.T) {
	storetest.Run(t, func(*testing.T) idempotency.Store { return memstore.New() })
}

func TestSweepRemovesOnlyExpiredKeys(t *testing.T) {
	s := memstore.New()
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, expires := range []time.Time{now.Add(-time.Second), now, now.Add(time.Second)} {
		_, _ = s.Claim(context.Background(), idempotency.Claim{Scope: "s", Key: string(rune('a' + i)), Lock: "l", Now: now, ExpiresAt: expires})
	}
	if n := s.Sweep(now); n != 2 || s.Len() != 1 {
		t.Fatalf("swept %d, %d left", n, s.Len())
	}
}
