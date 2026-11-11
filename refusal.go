package idempotency

import (
	"encoding/json"
	"net/http"
)

// Refusal is an answer the middleware gives itself, without running the
// handler. Code is stable and meant for clients to branch on.
type Refusal struct {
	Status  int
	Code    string
	Message string
}

// The refusals, by code.
const (
	CodeKeyInvalid  = "IDEMPOTENCY_KEY_INVALID"
	CodeKeyReused   = "IDEMPOTENCY_KEY_REUSED"
	CodeInProgress  = "IDEMPOTENCY_IN_PROGRESS"
	CodeBodyTooBig  = "IDEMPOTENCY_BODY_TOO_LARGE"
	CodeUnreadable  = "IDEMPOTENCY_BODY_UNREADABLE"
	CodeStoreFailed = "IDEMPOTENCY_STORE_FAILED"
)

var (
	refusalInvalidKey = Refusal{http.StatusBadRequest, CodeKeyInvalid,
		"Idempotency-Key must be one value of 1 to 255 visible ASCII characters."}
	refusalReused = Refusal{http.StatusUnprocessableEntity, CodeKeyReused,
		"This Idempotency-Key was already used for a different request."}
	refusalInProgress = Refusal{http.StatusConflict, CodeInProgress,
		"A request with this Idempotency-Key is still being processed."}
	refusalTooLarge = Refusal{http.StatusRequestEntityTooLarge, CodeBodyTooBig,
		"The request body is too large."}
	refusalUnreadable = Refusal{http.StatusBadRequest, CodeUnreadable,
		"The request body could not be read."}
	refusalStore = Refusal{http.StatusInternalServerError, CodeStoreFailed,
		"The request could not be checked for a repeat. Try again."}
)

// WriteRefusal is the default refusal writer:
//
//	{"error":{"code":"IDEMPOTENCY_KEY_REUSED","message":"…"}}
//
// Set Config.Refuse to answer in your API's own error format.
func WriteRefusal(w http.ResponseWriter, _ *http.Request, ref Refusal) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(ref.Status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": ref.Code, "message": ref.Message},
	})
}
