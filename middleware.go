package main

import (
	"crypto/subtle"
	"net/http"
)

const apiKeyHeader = "X-TestMO-Key"

// withCORS makes the API callable from the dashboard's browser origin.
//
// Origin is "*" because the API key header is the only credential — there are
// no cookies to leak to a hostile origin, so a permissive origin grants
// nothing that possessing the key doesn't already grant.
//
// Preflights are answered here, before auth: browsers never attach custom
// headers to an OPTIONS request, so requiring the key at this point would
// reject every legitimate browser call before it started.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		h.Set("Access-Control-Allow-Headers", apiKeyHeader)
		h.Set("Access-Control-Max-Age", "86400")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withAuth requires a matching X-TestMO-Key. The comparison is constant-time
// so response latency doesn't leak how much of the key a guess got right.
func withAuth(key string, next http.Handler) http.Handler {
	want := []byte(key)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get(apiKeyHeader))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}
