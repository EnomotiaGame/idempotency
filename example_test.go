package idempotency_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/EnomotiaGame/idempotency"
	"github.com/EnomotiaGame/idempotency/memstore"
)

func Example() {
	orders := 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /orders", func(w http.ResponseWriter, r *http.Request) {
		orders++
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"order":%d}`, orders)
	})

	guard := idempotency.New(idempotency.Config{
		Store: memstore.New(),
		// In a real server: the signed-in account, set by your auth middleware.
		Scope: func(r *http.Request) (string, bool) {
			user := r.Header.Get("X-User")
			return user, user != ""
		},
	})
	handler := guard(mux)

	send := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader(`{"item":"book"}`))
		r.Header.Set("X-User", "alice")
		r.Header.Set(idempotency.Header, "3f1c9a52-order-1")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}

	first := send()
	retry := send() // the client never saw the first answer, and tries again
	fmt.Println(first.Code, first.Body.String())
	fmt.Println(retry.Code, retry.Body.String(), retry.Header().Get(idempotency.ReplayedHeader))
	fmt.Println("orders placed:", orders)
	// Output:
	// 201 {"order":1}
	// 201 {"order":1} true
	// orders placed: 1
}
