package pgstore_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EnomotiaGame/idempotency"
	"github.com/EnomotiaGame/idempotency/pgstore"
	"github.com/EnomotiaGame/idempotency/storetest"
)

// The tests run against a real PostgreSQL when IDEMPOTENCY_TEST_DATABASE_URL
// names one, and skip otherwise. Each test makes its own table and drops it.
func pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("IDEMPOTENCY_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("IDEMPOTENCY_TEST_DATABASE_URL is not set")
	}
	p, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func freshStore(t *testing.T, p *pgxpool.Pool) *pgstore.Store {
	t.Helper()
	var b [6]byte
	_, _ = rand.Read(b[:])
	table := "idem_test_" + hex.EncodeToString(b[:])
	s, err := pgstore.New(p, pgstore.WithTable(table))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTable(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = p.Exec(context.Background(), "DROP TABLE "+table) })
	return s
}

func TestStore(t *testing.T) {
	p := pool(t)
	storetest.Run(t, func(t *testing.T) idempotency.Store { return freshStore(t, p) })
}

func TestSweepRemovesOnlyExpiredKeys(t *testing.T) {
	p := pool(t)
	s := freshStore(t, p)
	ctx := context.Background()
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, expires := range []time.Time{now.Add(-time.Second), now, now.Add(time.Second)} {
		created := expires.Add(-time.Hour)
		if _, err := s.Claim(ctx, idempotency.Claim{
			Scope: "s", Key: string(rune('a' + i)), Fingerprint: []byte{1}, Route: "r", Lock: "l",
			Now: created, ExpiresAt: expires, StaleBefore: created,
		}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.Sweep(ctx, now)
	if err != nil || n != 2 {
		t.Fatalf("swept %d, %v", n, err)
	}
	if _, found, _ := s.Get(ctx, "s", "c"); !found {
		t.Fatal("a living key was swept")
	}
}

func TestWithTableRefusesAnythingButAName(t *testing.T) {
	for _, bad := range []string{"", "a b", "t; DROP TABLE x", `"quoted"`, "a.b.c", "1table"} {
		if _, err := pgstore.New(nil, pgstore.WithTable(bad)); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}
	for _, good := range []string{"keys", "app.idem_keys", "_t1"} {
		if _, err := pgstore.New(nil, pgstore.WithTable(good)); err != nil {
			t.Fatalf("%q: %v", good, err)
		}
	}
}
