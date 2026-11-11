package idempotency_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EnomotiaGame/idempotency"
	"github.com/EnomotiaGame/idempotency/memstore"
)

const userHeader = "X-Test-User"

// harness is a handler behind the middleware that counts how often it ran and
// answers what answer says.
type harness struct {
	store  *memstore.Store
	now    time.Time
	ran    atomic.Int32
	answer func(w http.ResponseWriter, r *http.Request)
	h      http.Handler
}

func newHarness(t *testing.T, tweak ...func(*idempotency.Config)) *harness {
	t.Helper()
	hs := &harness{store: memstore.New(), now: time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)}
	hs.answer = func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"run": hs.ran.Load(), "echo": string(body)})
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hs.ran.Add(1)
		hs.answer(w, r)
	})
	cfg := idempotency.Config{
		Store: hs.store,
		Scope: func(r *http.Request) (string, bool) {
			u := r.Header.Get(userHeader)
			return u, u != ""
		},
		Now:    func() time.Time { return hs.now },
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Exempt: func(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/auth/") },
	}
	for _, f := range tweak {
		f(&cfg)
	}
	hs.h = idempotency.New(cfg)(next)
	return hs
}

// send makes a request as user ("" for nobody), with header pairs.
func (hs *harness) send(method, path, body, user string, headers ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if user != "" {
		r.Header.Set(userHeader, user)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Add(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	hs.h.ServeHTTP(rec, r)
	return rec
}

func errorCode(rec *httptest.ResponseRecorder) string {
	var out struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out.Error.Code
}

const key = idempotency.Header

// Without the header nothing changes: every request runs and nothing is kept.
func TestARequestWithoutTheHeaderRunsEveryTime(t *testing.T) {
	hs := newHarness(t)
	for i := 0; i < 3; i++ {
		rec := hs.send(http.MethodPost, "/orders", `{"a":1}`, "u7")
		if rec.Code != http.StatusCreated || rec.Header().Get(idempotency.ReplayedHeader) != "" {
			t.Fatalf("request %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	if hs.ran.Load() != 3 || hs.store.Len() != 0 {
		t.Fatalf("ran %d times, kept %d keys", hs.ran.Load(), hs.store.Len())
	}
}

// The same command twice is done once, and the retry hears exactly what the
// first attempt was told.
func TestTheSameKeyAndRequestIsReplayedNotRerun(t *testing.T) {
	hs := newHarness(t)
	first := hs.send(http.MethodPost, "/orders", `{"a":1}`, "u7", key, "k-1")
	second := hs.send(http.MethodPost, "/orders", `{"a":1}`, "u7", key, "k-1")
	if hs.ran.Load() != 1 {
		t.Fatalf("the handler ran %d times", hs.ran.Load())
	}
	if second.Code != first.Code || !bytes.Equal(second.Body.Bytes(), first.Body.Bytes()) {
		t.Fatalf("replay differs: %d %s vs %d %s", second.Code, second.Body, first.Code, first.Body)
	}
	if second.Header().Get(idempotency.ReplayedHeader) != "true" || first.Header().Get(idempotency.ReplayedHeader) != "" {
		t.Fatal("the replay is not marked, or the first answer is")
	}
	if ct := second.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("replayed content type %q", ct)
	}
}

// A refusal is an answer too, and the retry is told the same.
func TestARefusalIsKeptAndReplayed(t *testing.T) {
	hs := newHarness(t)
	hs.answer = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":{"code":"INSUFFICIENT_FUNDS"}}`)
	}
	hs.send(http.MethodPost, "/x", `{}`, "u7", key, "k")
	rec := hs.send(http.MethodPost, "/x", `{}`, "u7", key, "k")
	if hs.ran.Load() != 1 || rec.Code != http.StatusConflict || errorCode(rec) != "INSUFFICIENT_FUNDS" {
		t.Fatalf("ran %d, replay %d %s", hs.ran.Load(), rec.Code, rec.Body)
	}
}

// A key used for something else is a client bug, and it is told so instead of
// being handed the other request's answer.
func TestTheSameKeyOnAnotherRequestIsRefused(t *testing.T) {
	hs := newHarness(t)
	hs.send(http.MethodPost, "/accounts/1/transfers", `{"a":1}`, "u7", key, "k")
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/accounts/1/transfers", `{"a":2}`},
		{http.MethodPost, "/accounts/2/transfers", `{"a":1}`},
		{http.MethodPost, "/accounts/1/transfers?x=1", `{"a":1}`},
		{http.MethodPut, "/accounts/1/transfers", `{"a":1}`},
	} {
		rec := hs.send(c.method, c.path, c.body, "u7", key, "k")
		if rec.Code != http.StatusUnprocessableEntity || errorCode(rec) != idempotency.CodeKeyReused {
			t.Fatalf("%s %s %s: %d %s", c.method, c.path, c.body, rec.Code, rec.Body)
		}
	}
	if hs.ran.Load() != 1 {
		t.Fatalf("the handler ran %d times", hs.ran.Load())
	}
}

// While the first attempt runs, the second is turned away and nothing starts.
func TestASecondAttemptWhileTheFirstRunsIsTurnedAway(t *testing.T) {
	hs := newHarness(t)
	entered, release := make(chan struct{}), make(chan struct{})
	base := hs.answer
	hs.answer = func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		base(w, r)
	}
	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- hs.send(http.MethodPost, "/x", `{}`, "u7", key, "k") }()
	<-entered
	rec := hs.send(http.MethodPost, "/x", `{}`, "u7", key, "k")
	if rec.Code != http.StatusConflict || errorCode(rec) != idempotency.CodeInProgress {
		t.Fatalf("second attempt: %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("the in-progress answer says nothing about when to try again")
	}
	close(release)
	if first := <-done; first.Code != http.StatusCreated || hs.ran.Load() != 1 {
		t.Fatalf("first: %d, ran %d", first.Code, hs.ran.Load())
	}
	if again := hs.send(http.MethodPost, "/x", `{}`, "u7", key, "k"); again.Header().Get(idempotency.ReplayedHeader) != "true" {
		t.Fatalf("after the first finished: %d %s", again.Code, again.Body)
	}
}

// A server fault, a rate limit, a timeout or a client that left is not the
// command's answer. The key is forgotten, and the retry runs.
func TestAnAnswerThatSaysNothingAboutTheCommandIsNotKept(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusServiceUnavailable,
		http.StatusTooManyRequests, http.StatusRequestTimeout, idempotency.StatusClientClosedRequest} {
		hs := newHarness(t)
		code := status
		hs.answer = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }
		hs.send(http.MethodPost, "/x", `{}`, "u7", key, "k")
		code = http.StatusOK
		rec := hs.send(http.MethodPost, "/x", `{}`, "u7", key, "k")
		if hs.ran.Load() != 2 || rec.Code != http.StatusOK || rec.Header().Get(idempotency.ReplayedHeader) != "" {
			t.Fatalf("after a %d: ran %d, retry %d", status, hs.ran.Load(), rec.Code)
		}
	}
}

// A panic leaves no key behind, and carries on to the recoverer.
func TestAPanicReleasesTheKey(t *testing.T) {
	hs := newHarness(t)
	base := hs.answer
	hs.answer = func(http.ResponseWriter, *http.Request) { panic("boom") }
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the panic was swallowed")
			}
		}()
		hs.send(http.MethodPost, "/x", `{}`, "u7", key, "k")
	}()
	hs.answer = base
	if rec := hs.send(http.MethodPost, "/x", `{}`, "u7", key, "k"); rec.Code != http.StatusCreated || hs.ran.Load() != 2 {
		t.Fatalf("after a panic: %d, ran %d", rec.Code, hs.ran.Load())
	}
}

// Keys are a scope's own: two users who choose the same key never meet.
func TestKeysArePerScope(t *testing.T) {
	hs := newHarness(t)
	hs.send(http.MethodPost, "/x", `{}`, "u7", key, "k")
	rec := hs.send(http.MethodPost, "/x", `{}`, "u8", key, "k")
	if hs.ran.Load() != 2 || rec.Header().Get(idempotency.ReplayedHeader) != "" {
		t.Fatalf("another scope's key: ran %d, %d", hs.ran.Load(), rec.Code)
	}
}

// A key is remembered for Retention. After that the same name is a new command.
func TestAKeyIsForgottenAfterRetention(t *testing.T) {
	hs := newHarness(t, func(c *idempotency.Config) { c.Retention = time.Hour })
	hs.send(http.MethodPost, "/x", `{}`, "u7", key, "k")
	hs.now = hs.now.Add(time.Hour - time.Second)
	if rec := hs.send(http.MethodPost, "/x", `{}`, "u7", key, "k"); rec.Header().Get(idempotency.ReplayedHeader) != "true" {
		t.Fatalf("within the hour: %d", rec.Code)
	}
	hs.now = hs.now.Add(time.Second)
	if rec := hs.send(http.MethodPost, "/x", `{"other":1}`, "u7", key, "k"); rec.Code != http.StatusCreated || hs.ran.Load() != 2 {
		t.Fatalf("after the hour: %d, ran %d", rec.Code, hs.ran.Load())
	}
}

// A holder that died is taken over by the same request after StaleAfter, and
// never by another one.
func TestAStaleHolderIsTakenOverOnlyByTheSameRequest(t *testing.T) {
	hs := newHarness(t)
	fp := idempotency.Fingerprint(http.MethodPost, "/x", "", []byte(`{}`))
	if ok, _ := hs.store.Claim(context.Background(), idempotency.Claim{
		Scope: "u7", Key: "k", Fingerprint: fp, Lock: "dead", Now: hs.now,
		ExpiresAt: hs.now.Add(idempotency.DefaultRetention), StaleBefore: hs.now.Add(-idempotency.DefaultStaleAfter),
	}); !ok {
		t.Fatal("could not plant a running request")
	}
	if rec := hs.send(http.MethodPost, "/x", `{}`, "u7", key, "k"); rec.Code != http.StatusConflict {
		t.Fatalf("a fresh holder: %d", rec.Code)
	}
	hs.now = hs.now.Add(idempotency.DefaultStaleAfter)
	if rec := hs.send(http.MethodPost, "/x", `{"different":1}`, "u7", key, "k"); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a stale holder, another request: %d", rec.Code)
	}
	if rec := hs.send(http.MethodPost, "/x", `{}`, "u7", key, "k"); rec.Code != http.StatusCreated || hs.ran.Load() != 1 {
		t.Fatalf("a stale holder, the same request: %d, ran %d", rec.Code, hs.ran.Load())
	}
}

func TestAMalformedKeyIsRefused(t *testing.T) {
	hs := newHarness(t)
	for _, headers := range [][]string{
		{key, strings.Repeat("a", idempotency.MaxKeyLen+1)},
		{key, "has space"},
		{key, "ünicode"},
		{key, "a", key, "b"},
	} {
		rec := hs.send(http.MethodPost, "/x", `{}`, "u7", headers...)
		if rec.Code != http.StatusBadRequest || errorCode(rec) != idempotency.CodeKeyInvalid {
			t.Fatalf("%q: %d %s", headers, rec.Code, rec.Body)
		}
	}
	if hs.ran.Load() != 0 {
		t.Fatalf("a refused key ran the command %d times", hs.ran.Load())
	}
	if rec := hs.send(http.MethodPost, "/x", `{}`, "u7", key, strings.Repeat("a", idempotency.MaxKeyLen)); rec.Code != http.StatusCreated {
		t.Fatalf("the longest key: %d", rec.Code)
	}
}

// Reads, exempt routes, uploads and requests with no scope ignore the header.
func TestTheHeaderIsIgnoredWhereItDoesNotApply(t *testing.T) {
	hs := newHarness(t)
	upload := func() {
		r := httptest.NewRequest(http.MethodPost, "/uploads", strings.NewReader("--b--"))
		r.Header.Set("Content-Type", "multipart/form-data; boundary=b")
		r.Header.Set(key, "u")
		r.Header.Set(userHeader, "u7")
		hs.h.ServeHTTP(httptest.NewRecorder(), r)
	}
	for i := 0; i < 2; i++ {
		hs.send(http.MethodGet, "/x", "", "u7", key, "g")
		hs.send(http.MethodPost, "/auth/password", `{}`, "u7", key, "a")
		hs.send(http.MethodPost, "/x", `{}`, "", key, "anon")
		upload()
	}
	if hs.ran.Load() != 8 || hs.store.Len() != 0 {
		t.Fatalf("ran %d, kept %d", hs.ran.Load(), hs.store.Len())
	}
}

// The middleware reads the body to hash it, and hands the same bytes on.
func TestTheHandlerStillReadsTheBody(t *testing.T) {
	hs := newHarness(t)
	rec := hs.send(http.MethodPost, "/x", `{"item":"book"}`, "u7", key, "k")
	var out struct {
		Echo string `json:"echo"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Echo != `{"item":"book"}` {
		t.Fatalf("the handler read %q", out.Echo)
	}
}

func TestABodyOverTheLimitIsRefused(t *testing.T) {
	hs := newHarness(t, func(c *idempotency.Config) { c.MaxBody = 8 })
	if rec := hs.send(http.MethodPost, "/x", `{"a":"12345"}`, "u7", key, "k"); rec.Code != http.StatusRequestEntityTooLarge ||
		errorCode(rec) != idempotency.CodeBodyTooBig || hs.ran.Load() != 0 {
		t.Fatalf("an oversized body: %d %s, ran %d", rec.Code, rec.Body, hs.ran.Load())
	}
	if rec := hs.send(http.MethodPost, "/x", `{"a":1}`, "u7", key, "k2"); rec.Code != http.StatusCreated {
		t.Fatalf("a body within the limit: %d", rec.Code)
	}
}

type brokenStore struct{ *memstore.Store }

func (brokenStore) Claim(context.Context, idempotency.Claim) (bool, error) {
	return false, errors.New("database is down")
}

// A store that fails does not let the command run unguarded.
func TestAFailingStoreRefusesWithoutRunning(t *testing.T) {
	hs := newHarness(t, func(c *idempotency.Config) { c.Store = brokenStore{memstore.New()} })
	rec := hs.send(http.MethodPost, "/x", `{}`, "u7", key, "k")
	if rec.Code != http.StatusInternalServerError || errorCode(rec) != idempotency.CodeStoreFailed || hs.ran.Load() != 0 {
		t.Fatalf("a failing store: %d %s, ran %d", rec.Code, rec.Body, hs.ran.Load())
	}
}

// Refusals can be written in the API's own error format.
func TestRefusalsCanBeWrittenYourWay(t *testing.T) {
	hs := newHarness(t, func(c *idempotency.Config) {
		c.Refuse = func(w http.ResponseWriter, _ *http.Request, ref idempotency.Refusal) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(ref.Status)
			_ = json.NewEncoder(w).Encode(map[string]string{"type": ref.Code, "title": ref.Message})
		}
	})
	rec := hs.send(http.MethodPost, "/x", `{}`, "u7", key, "has space")
	if rec.Code != http.StatusBadRequest || rec.Header().Get("Content-Type") != "application/problem+json" ||
		!strings.Contains(rec.Body.String(), idempotency.CodeKeyInvalid) {
		t.Fatalf("a custom refusal: %d %s %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
}

func TestNewWithoutStoreOrScopePanics(t *testing.T) {
	for _, cfg := range []idempotency.Config{
		{Scope: func(*http.Request) (string, bool) { return "", true }},
		{Store: memstore.New()},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("New(%+v) did not panic", cfg)
				}
			}()
			idempotency.New(cfg)
		}()
	}
}

func TestFingerprintSeparatesItsParts(t *testing.T) {
	a := idempotency.Fingerprint("POST", "/ab", "", nil)
	b := idempotency.Fingerprint("POST", "/a", "b", nil)
	if bytes.Equal(a, b) || len(a) != 32 {
		t.Fatal("the path and the query run together")
	}
}
