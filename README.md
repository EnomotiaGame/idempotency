# idempotency

[![Go Reference](https://pkg.go.dev/badge/github.com/EnomotiaGame/idempotency.svg)](https://pkg.go.dev/github.com/EnomotiaGame/idempotency)
[![CI](https://github.com/EnomotiaGame/idempotency/actions/workflows/ci.yml/badge.svg)](https://github.com/EnomotiaGame/idempotency/actions/workflows/ci.yml)

`net/http` middleware for the `Idempotency-Key` header: a retried request gets the first answer back instead of running twice.

A phone loses answers. The user taps "pay", the request reaches your server, the payment goes through, and the response dies in a tunnel. The client cannot tell a lost answer from a lost request, so it retries. Without idempotency, the retry is a second payment, or an "already paid" error for something that worked.

The middleware began in [ENOMOTIA](https://enomotia.net), a browser and mobile strategy game, where flaky mobile connections were sending "build" and "attack" twice. We extracted it because nothing we found handled all of this:

- **Byte-for-byte replay.** A finished request's status, `Content-Type` and body are stored and replayed, marked `Idempotent-Replayed: true`. Refusals are replayed too: "insufficient funds" is the command's answer.
- **Request fingerprinting.** The key is bound to a SHA-256 of the method, path, query and body. Reusing a key for a different request is a client bug. It gets `422`, never someone else's answer.
- **Only real answers are kept.** A `5xx`, `429`, `408` or `499`, or a panic, says nothing about the command. The key is released and the retry runs afresh.
- **Concurrent duplicates wait.** While the first attempt runs, a second gets `409` with `Retry-After` and nothing starts.
- **Dead holders are taken over safely.** If a process dies mid-request, the same request may take the key over after a timeout. A different request may not. A late answer from the dead holder cannot overwrite the new one.
- **Scoped keys.** Keys live per account, tenant or API key, so two users who pick the same key never meet.
- **Opt-in per call.** Requests without the header, reads, multipart uploads and unauthenticated requests pass through untouched. Clients can adopt it one endpoint at a time.

The semantics follow the IETF draft [The Idempotency-Key HTTP Header Field](https://datatracker.ietf.org/doc/draft-ietf-httpapi-idempotency-key-header/).

The core module has **no dependencies**. Postgres support is a separate module, so you only pull in `pgx` if you use it.

## Install

```sh
go get github.com/EnomotiaGame/idempotency
go get github.com/EnomotiaGame/idempotency/pgstore   # optional, PostgreSQL store
```

Go 1.23 or later.

## Use

```go
import (
	"github.com/EnomotiaGame/idempotency"
	"github.com/EnomotiaGame/idempotency/pgstore"
)

store, err := pgstore.New(pool) // a *pgxpool.Pool
if err != nil { … }

guard := idempotency.New(idempotency.Config{
	Store: store,
	// The namespace keys live in. Put the middleware behind your auth.
	Scope: func(r *http.Request) (string, bool) {
		user, ok := auth.UserFrom(r.Context())
		return user.ID, ok
	},
})

http.ListenAndServe(":8080", authenticate(guard(mux)))
```

The client sends a fresh key per command and the same key on every retry of that command:

```http
POST /orders HTTP/1.1
Idempotency-Key: 3f1c9a52-6e0b-4a6f-9d2c-1b7f0f0e8a11
Content-Type: application/json

{"item":"book","qty":1}
```

## What the client gets

| Situation | Answer |
|---|---|
| No header, a `GET`/`HEAD`/`OPTIONS`, a multipart upload, an exempt route, no scope | The handler runs as usual |
| New key | The handler runs; an answer under 500 (except 408, 429, 499) is kept |
| Same key, same request, still running | `409 IDEMPOTENCY_IN_PROGRESS`, `Retry-After: 1` |
| Same key, same request, finished | The kept answer, with `Idempotent-Replayed: true` |
| Same key, different request | `422 IDEMPOTENCY_KEY_REUSED` |
| Malformed key (not 1–255 visible ASCII characters, or sent twice) | `400 IDEMPOTENCY_KEY_INVALID` |
| Body over `MaxBody` | `413 IDEMPOTENCY_BODY_TOO_LARGE` |
| The store is down | `500 IDEMPOTENCY_STORE_FAILED`; the handler does **not** run |

Refusals are JSON by default (`{"error":{"code":…,"message":…}}`). Set `Config.Refuse` to write them in your API's own error format, such as RFC 9457 problem details.

## Configuration

| Field | Default | |
|---|---|---|
| `Store` | required | Where keys live |
| `Scope` | required | The namespace for a request's keys; `false` passes the request through |
| `Exempt` | none | Requests whose header is ignored (sign-in, for example) |
| `Retention` | 24h | How long a key is remembered |
| `StaleAfter` | 2m | When a silent holder counts as dead. Keep it above your server's write timeout |
| `MaxBody` | 1 MiB | The largest body hashed |
| `Now`, `Log`, `Refuse` | `time.Now`, `slog.Default()`, `WriteRefusal` | |

## Stores

- **`memstore`**: in memory. For tests and single-process servers. Call `Sweep` periodically.
- **`pgstore`**: PostgreSQL through pgx. Each rule is one SQL statement, so any number of instances can share it. Create the table with `pgstore.Schema` in your migrations (or `CreateTable`), and call `Sweep` from a periodic job.
- **Your own** (Redis, DynamoDB, …): implement the four-method `idempotency.Store` and run the conformance suite against it:

```go
func TestMyStore(t *testing.T) {
	storetest.Run(t, func(t *testing.T) idempotency.Store { return newMyStore(t) })
}
```

## The one window it cannot close

The handler commits its own work, and the answer is stored afterwards in a separate call. If the process dies between the two, the key stays "running". After `StaleAfter`, a retry of the same request runs the handler again. Closing that window means writing the answer inside the handler's own transaction, and this middleware does not reach into your transactions. In practice the window is a crash between two adjacent statements. For operations where even that is unacceptable, also enforce uniqueness in your domain, with a unique constraint on a payment reference.

## Development

```sh
go test ./...
IDEMPOTENCY_TEST_DATABASE_URL=postgres://localhost/idempotency_test go test ./pgstore/...
```

The `pgstore` tests skip without a database. Each test makes its own table and drops it.

## License

MIT © TRUETHINKS LTD
