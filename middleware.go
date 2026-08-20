package main

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

const apiKeyHeader = "X-TestMO-Key"

// withCORS makes the API callable from the dashboard's browser origin.
//
// The dashboard is a static site on its own domain and every customer's
// browser calls each node's agent directly, so every legitimate request is
// cross-origin. Without these headers the browser still makes the request but
// refuses to hand the response to the page.
//
// Preflights are answered here, before auth: browsers never attach custom
// headers to an OPTIONS request, so requiring the key at this point would
// reject every legitimate browser call before it started.
func withCORS(allowed []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()

		// Which origin comes back depends on the request's own Origin, so any
		// cache in front of this has to key on it. Sent unconditionally —
		// including in "*" mode, where it costs nothing — rather than being
		// one more branch that can be got wrong later.
		h.Add("Vary", "Origin")

		if origin, ok := allowedOrigin(allowed, r.Header.Get("Origin")); ok {
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			h.Set("Access-Control-Allow-Headers", apiKeyHeader)
			h.Set("Access-Control-Max-Age", "86400")
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// allowedOrigin returns the value for Access-Control-Allow-Origin and whether
// to send the header at all.
//
// An empty allowlist means "*", unconditionally — the behaviour from before
// this field existed. That is what keeps an in-place binary swap from cutting
// off a dashboard whose agent.yaml predates it.
//
// With an allowlist, the request's own Origin is echoed back, because the
// header holds exactly one origin: a list is not valid there, and "*" would
// make the allowlist decorative. A non-matching origin gets no header rather
// than a rejection — the browser blocks the read by itself, while a 403 would
// break every caller that sends no Origin at all, curl included.
func allowedOrigin(allowed []string, origin string) (string, bool) {
	if len(allowed) == 0 {
		return "*", true
	}
	if origin == "" {
		return "", false
	}
	for _, a := range allowed {
		// Browsers lowercase the scheme and host, but a hand-written config
		// entry might not, and the mismatch would be invisible from here.
		if strings.EqualFold(a, origin) {
			return origin, true
		}
	}
	return "", false
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
