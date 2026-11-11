/*
Package idempotency is HTTP middleware that makes a retried request answer
instead of acting twice.

A phone loses answers. The user taps "pay", the request reaches the server and
the payment goes through, and the answer dies in a tunnel. The client cannot
tell a lost answer from a lost request, so it sends the request again. Without
idempotency the second request is a second payment, or a refusal ("already
paid") that the client shows as an error for something that worked.

# The client names the command

A state-changing request may carry an Idempotency-Key header: an opaque name the
client makes for one command (a UUID is the obvious choice) and keeps for every
retry of it. Per scope (usually the signed-in account) and per key, the server
remembers for a while (Config.Retention, a day by default):

  - nothing yet: the request runs, and its answer is kept;
  - the same request, still running: 409 IDEMPOTENCY_IN_PROGRESS with
    Retry-After, and nothing starts;
  - the same request, finished: the kept answer, status and body byte for
    byte, with "Idempotent-Replayed: true". The handler does not run again;
  - another request: 422 IDEMPOTENCY_KEY_REUSED.

"The same request" is the method, the path, the query and the body, hashed
(Fingerprint). A key reused for another amount or another resource is a client
bug, and replaying the first answer to it would be a lie.

# What is kept, and what is not

An answer under 500 is the command's own answer: "done", or "refused, and here
is why". A retry gets it again, refusals included. A 5xx, a 429, a 408 or a
request whose client went away (499) says nothing about the command, so the
key is forgotten and a retry runs afresh. A panic in the handler forgets the
key too, and the panic carries on to your recoverer.

# The header is optional

A request without the header goes through untouched, and so do reads (GET,
HEAD, OPTIONS), multipart uploads, requests Config.Exempt names, and requests
with no scope (Config.Scope returns false, for example nobody is signed in).
A client can adopt the header one call at a time.

# A dead holder

The handler commits its own work and the answer is written after it, in a
store call of its own. If the process dies between the two, the key stays
"running". After Config.StaleAfter a retry of the same request may take it over
and run again. That window, a crash between a commit and the next statement,
is the one this middleware cannot close without the handler writing the answer
inside its own transaction.

# Stores

The core has no dependencies. memstore keeps keys in memory, which suits tests
and a single process. pgstore (a module of its own) keeps them in PostgreSQL.
Any other store can be checked with storetest.
*/
package idempotency

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Header is the request header a client names a command with.
const Header = "Idempotency-Key"

// ReplayedHeader marks a replayed answer.
const ReplayedHeader = "Idempotent-Replayed"

// Defaults for the Config fields left zero.
const (
	DefaultRetention  = 24 * time.Hour
	DefaultStaleAfter = 2 * time.Minute
	DefaultMaxBody    = 1 << 20
)

// MaxKeyLen is the longest key accepted.
const MaxKeyLen = 255

// StatusClientClosedRequest is the status a handler may write when its client
// went away (nginx's 499). Such an answer is never kept.
const StatusClientClosedRequest = 499

// Config wires the middleware. Store and Scope are required.
type Config struct {
	Store Store
	// Scope names the namespace a request's keys live in, usually the signed-in
	// account. A request with no scope (ok false) passes through untouched. Put
	// the middleware behind authentication so the scope is known.
	Scope func(r *http.Request) (scope string, ok bool)
	// Exempt names requests whose header is ignored. Multipart bodies are
	// always exempt.
	Exempt func(r *http.Request) bool
	// Retention is how long a key is remembered after its first use.
	Retention time.Duration
	// StaleAfter is how long a request may hold a key without finishing before
	// a retry of the same request may take it over. Make it longer than your
	// server's write timeout.
	StaleAfter time.Duration
	// MaxBody is the largest body hashed; a larger one is refused with 413.
	MaxBody int64
	// Now is the clock. time.Now when nil.
	Now func() time.Time
	// Log receives store failures. slog.Default when nil.
	Log *slog.Logger
	// Refuse writes the middleware's own refusals. WriteRefusal when nil.
	Refuse func(w http.ResponseWriter, r *http.Request, ref Refusal)
}

// New returns the Idempotency-Key middleware. It panics if Store or Scope is
// missing, because a middleware without them could only pretend.
func New(cfg Config) func(http.Handler) http.Handler {
	if cfg.Store == nil || cfg.Scope == nil {
		panic("idempotency: Config.Store and Config.Scope are required")
	}
	if cfg.Retention <= 0 {
		cfg.Retention = DefaultRetention
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = DefaultStaleAfter
	}
	if cfg.MaxBody <= 0 {
		cfg.MaxBody = DefaultMaxBody
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Refuse == nil {
		cfg.Refuse = WriteRefusal
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			values := r.Header.Values(Header)
			if len(values) == 0 || !isCommand(r.Method) || isMultipart(r) ||
				(cfg.Exempt != nil && cfg.Exempt(r)) {
				next.ServeHTTP(w, r)
				return
			}
			scope, ok := cfg.Scope(r)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			key, ok := validKey(values)
			if !ok {
				cfg.Refuse(w, r, refusalInvalidKey)
				return
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, cfg.MaxBody+1))
			if err != nil {
				cfg.Refuse(w, r, refusalUnreadable)
				return
			}
			if int64(len(body)) > cfg.MaxBody {
				cfg.Refuse(w, r, refusalTooLarge)
				return
			}
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(body))

			now := cfg.Now()
			claim := Claim{
				Scope:       scope,
				Key:         key,
				Fingerprint: Fingerprint(r.Method, r.URL.Path, r.URL.RawQuery, body),
				Route:       route(r),
				Lock:        newLock(),
				Now:         now,
				ExpiresAt:   now.Add(cfg.Retention),
				StaleBefore: now.Add(-cfg.StaleAfter),
			}
			held, err := cfg.Store.Claim(r.Context(), claim)
			if err != nil {
				storeFailed(cfg, w, r, "claim", err)
				return
			}
			if !held {
				answerHeld(cfg, w, r, claim)
				return
			}
			run(cfg, next, w, r, claim)
		})
	}
}

// answerHeld answers a request whose key somebody already has.
func answerHeld(cfg Config, w http.ResponseWriter, r *http.Request, claim Claim) {
	rec, found, err := cfg.Store.Get(r.Context(), claim.Scope, claim.Key)
	if err != nil {
		storeFailed(cfg, w, r, "get", err)
		return
	}
	if !found {
		// Released between the claim and the read: the first request is over
		// and left nothing to replay. Say "busy" and let the client's next
		// retry take the key; running here would race it.
		inProgress(cfg, w, r)
		return
	}
	if !bytes.Equal(rec.Fingerprint, claim.Fingerprint) {
		cfg.Refuse(w, r, refusalReused)
		return
	}
	if rec.Status == 0 {
		inProgress(cfg, w, r)
		return
	}
	h := w.Header()
	if rec.ContentType != "" {
		h.Set("Content-Type", rec.ContentType)
	}
	h.Set(ReplayedHeader, "true")
	w.WriteHeader(rec.Status)
	_, _ = w.Write(rec.Body)
}

func inProgress(cfg Config, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Retry-After", "1")
	cfg.Refuse(w, r, refusalInProgress)
}

func storeFailed(cfg Config, w http.ResponseWriter, r *http.Request, op string, err error) {
	cfg.Log.ErrorContext(r.Context(), "idempotency: store failed", "op", op, "route", route(r), "error", err)
	cfg.Refuse(w, r, refusalStore)
}

// run executes the command and keeps its answer.
func run(cfg Config, next http.Handler, w http.ResponseWriter, r *http.Request, claim Claim) {
	// The answer is kept whatever happens to the client's connection: a client
	// that left is the client most likely to retry.
	keep := context.WithoutCancel(r.Context())
	cw := &capture{ResponseWriter: w}
	defer func() {
		if p := recover(); p != nil {
			release(cfg, keep, claim)
			panic(p)
		}
	}()
	next.ServeHTTP(cw, r)
	status := cw.status
	if status == 0 {
		status = http.StatusOK
	}
	if !keepable(status) {
		release(cfg, keep, claim)
		return
	}
	rec := Record{
		Fingerprint: claim.Fingerprint,
		Status:      status,
		ContentType: cw.Header().Get("Content-Type"),
		Body:        cw.buf.Bytes(),
	}
	ok, err := cfg.Store.Complete(keep, claim.Scope, claim.Key, claim.Lock, rec, cfg.Now())
	switch {
	case err != nil:
		cfg.Log.ErrorContext(keep, "idempotency: answer not kept", "route", claim.Route, "error", err)
	case !ok:
		cfg.Log.WarnContext(keep, "idempotency: answer not kept, the key was taken over", "route", claim.Route)
	}
}

func release(cfg Config, ctx context.Context, claim Claim) {
	if err := cfg.Store.Release(ctx, claim.Scope, claim.Key, claim.Lock); err != nil {
		cfg.Log.ErrorContext(ctx, "idempotency: key not released", "route", claim.Route, "error", err)
	}
}

// keepable says an answer is the command's own: anything under 500 except a
// rate limit, a timeout and a client that left. Those say nothing about
// whether the command can be done, and a retry should run it.
func keepable(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusRequestTimeout, StatusClientClosedRequest:
		return false
	}
	return status < 500
}

func isCommand(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

func isMultipart(r *http.Request) bool {
	return strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/")
}

// validKey reads the header: exactly one value of 1 to MaxKeyLen visible
// ASCII characters.
func validKey(values []string) (string, bool) {
	if len(values) != 1 {
		return "", false
	}
	k := values[0]
	if len(k) == 0 || len(k) > MaxKeyLen {
		return "", false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7e {
			return "", false
		}
	}
	return k, true
}

// Fingerprint is what makes two requests "the same": the SHA-256 of the
// method, the path, the raw query and the body. A client's own retry sends the
// same bytes; anything else differs.
func Fingerprint(method, path, query string, body []byte) []byte {
	h := sha256.New()
	for _, part := range []string{method, path, query} {
		_, _ = io.WriteString(h, part)
		_, _ = h.Write([]byte{0})
	}
	_, _ = h.Write(body)
	return h.Sum(nil)
}

func route(r *http.Request) string {
	if r.Pattern != "" {
		return r.Pattern
	}
	return r.Method + " " + r.URL.Path
}

func newLock() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// capture passes the answer through and keeps a copy of it.
type capture struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (c *capture) WriteHeader(code int) {
	if c.status == 0 && code >= 200 {
		c.status = code
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *capture) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	c.buf.Write(b)
	return c.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (c *capture) Unwrap() http.ResponseWriter { return c.ResponseWriter }
