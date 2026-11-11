// Package pgstore keeps idempotency keys in PostgreSQL, through pgx.
//
// Create the table once, with your migrations or with CreateTable:
//
//	CREATE TABLE idempotency_keys (…)   -- see Schema
//
// then hand New a *pgxpool.Pool, a *pgx.Conn or a pgx.Tx. Every rule the
// middleware relies on is one SQL statement, so any number of server
// instances can share the table. Call Sweep now and then (an hourly job is
// plenty) to delete expired keys.
package pgstore

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/EnomotiaGame/idempotency"
)

// DB is what the store needs from pgx: a pool, a connection or a transaction.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// DefaultTable is the table New uses unless told otherwise.
const DefaultTable = "idempotency_keys"

// Schema is the DDL for the default table. SchemaFor gives it for another name.
var Schema = SchemaFor(DefaultTable)

// SchemaFor returns the DDL for a table of the given name.
func SchemaFor(table string) string {
	return strings.NewReplacer("{{table}}", table).Replace(`CREATE TABLE IF NOT EXISTS {{table}} (
    scope         TEXT        NOT NULL,
    key           TEXT        NOT NULL CHECK (char_length(key) BETWEEN 1 AND 255),
    -- SHA-256 of the request: method, path, query and body.
    fingerprint   BYTEA       NOT NULL,
    -- The route the request reached, for an operator; never compared.
    route         TEXT        NOT NULL,
    lock_id       TEXT        NOT NULL,
    -- NULL while the first request is still running.
    status        SMALLINT    CHECK (status BETWEEN 100 AND 599),
    content_type  TEXT,
    body          BYTEA,
    locked_at     TIMESTAMPTZ NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL,
    completed_at  TIMESTAMPTZ,
    expires_at    TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (scope, key),
    CHECK (expires_at > created_at),
    CHECK ((status IS NULL) = (completed_at IS NULL))
);
CREATE INDEX IF NOT EXISTS {{table}}_expires_at ON {{table}} (expires_at);
`)
}

// Option changes a Store.
type Option func(*Store) error

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}(\.[A-Za-z_][A-Za-z0-9_]{0,62})?$`)

// WithTable uses another table, optionally schema-qualified ("app.idem_keys").
func WithTable(name string) Option {
	return func(s *Store) error {
		if !identifier.MatchString(name) {
			return fmt.Errorf("pgstore: %q is not a plain table name", name)
		}
		s.table = name
		return nil
	}
}

// Store is an idempotency.Store on a PostgreSQL table.
type Store struct {
	db    DB
	table string

	claimSQL, getSQL, completeSQL, releaseSQL, sweepSQL string
}

// New returns a store on db.
func New(db DB, opts ...Option) (*Store, error) {
	s := &Store{db: db, table: DefaultTable}
	for _, o := range opts {
		if err := o(s); err != nil {
			return nil, err
		}
	}
	t := strings.NewReplacer("{{table}}", s.table)
	// A row is taken over when its time is up, whatever it was; or when it is
	// still running, its holder has been silent since $8, and the request is
	// the same one. A different request on a stale key is the client's mistake.
	s.claimSQL = t.Replace(`INSERT INTO {{table}} AS k (
    scope, key, fingerprint, route, lock_id, locked_at, created_at, expires_at
) VALUES ($1, $2, $3, $4, $5, $6, $6, $7)
ON CONFLICT (scope, key) DO UPDATE
   SET fingerprint  = EXCLUDED.fingerprint,
       route        = EXCLUDED.route,
       lock_id      = EXCLUDED.lock_id,
       status       = NULL,
       content_type = NULL,
       body         = NULL,
       locked_at    = EXCLUDED.locked_at,
       created_at   = EXCLUDED.created_at,
       completed_at = NULL,
       expires_at   = EXCLUDED.expires_at
 WHERE k.expires_at <= $6
    OR (k.status IS NULL AND k.locked_at <= $8 AND k.fingerprint = EXCLUDED.fingerprint)
RETURNING lock_id`)
	s.getSQL = t.Replace(`SELECT fingerprint, status, content_type, body FROM {{table}} WHERE scope = $1 AND key = $2`)
	s.completeSQL = t.Replace(`UPDATE {{table}}
   SET status = $1, content_type = $2, body = $3, completed_at = $4
 WHERE scope = $5 AND key = $6 AND lock_id = $7 AND status IS NULL`)
	s.releaseSQL = t.Replace(`DELETE FROM {{table}} WHERE scope = $1 AND key = $2 AND lock_id = $3 AND status IS NULL`)
	s.sweepSQL = t.Replace(`DELETE FROM {{table}} WHERE expires_at <= $1`)
	return s, nil
}

// CreateTable runs the store's DDL. Prefer your own migrations in production.
func (s *Store) CreateTable(ctx context.Context) error {
	if _, err := s.db.Exec(ctx, SchemaFor(s.table)); err != nil {
		return fmt.Errorf("pgstore: create table: %w", err)
	}
	return nil
}

// Claim implements idempotency.Store.
func (s *Store) Claim(ctx context.Context, c idempotency.Claim) (bool, error) {
	var lock string
	err := s.db.QueryRow(ctx, s.claimSQL,
		c.Scope, c.Key, c.Fingerprint, c.Route, c.Lock, c.Now, c.ExpiresAt, c.StaleBefore,
	).Scan(&lock)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("pgstore: claim: %w", err)
	}
	return true, nil
}

// Get implements idempotency.Store.
func (s *Store) Get(ctx context.Context, scope, key string) (idempotency.Record, bool, error) {
	var (
		rec         idempotency.Record
		status      *int16
		contentType *string
	)
	err := s.db.QueryRow(ctx, s.getSQL, scope, key).Scan(&rec.Fingerprint, &status, &contentType, &rec.Body)
	if errors.Is(err, pgx.ErrNoRows) {
		return idempotency.Record{}, false, nil
	}
	if err != nil {
		return idempotency.Record{}, false, fmt.Errorf("pgstore: get: %w", err)
	}
	if status != nil {
		rec.Status = int(*status)
	}
	if contentType != nil {
		rec.ContentType = *contentType
	}
	return rec, true, nil
}

// Complete implements idempotency.Store.
func (s *Store) Complete(ctx context.Context, scope, key, lock string, rec idempotency.Record, now time.Time) (bool, error) {
	body := rec.Body
	if body == nil {
		body = []byte{}
	}
	var contentType *string
	if rec.ContentType != "" {
		contentType = &rec.ContentType
	}
	tag, err := s.db.Exec(ctx, s.completeSQL, int16(rec.Status), contentType, body, now, scope, key, lock)
	if err != nil {
		return false, fmt.Errorf("pgstore: complete: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// Release implements idempotency.Store.
func (s *Store) Release(ctx context.Context, scope, key, lock string) error {
	if _, err := s.db.Exec(ctx, s.releaseSQL, scope, key, lock); err != nil {
		return fmt.Errorf("pgstore: release: %w", err)
	}
	return nil
}

// Sweep deletes the keys that expired at or before now and reports how many.
func (s *Store) Sweep(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.db.Exec(ctx, s.sweepSQL, now)
	if err != nil {
		return 0, fmt.Errorf("pgstore: sweep: %w", err)
	}
	return tag.RowsAffected(), nil
}

var _ idempotency.Store = (*Store)(nil)
