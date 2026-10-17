package api

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

const csp = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

func securityHeaders(hsts bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		if hsts {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

func noStoreAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// recoverer turns a handler panic into a generic 500 problem. The panic value is logged, never sent.
func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				slog.Error("panic in handler", "panic", v, "path", r.URL.Path)
				WriteProblem(w, http.StatusInternalServerError, "Internal error", "")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// validID reports whether id is a canonical lowercase UUID. Handlers must check this before any
// store lookup: a malformed id would otherwise surface as a Postgres cast error (a 500), and it
// must be a 404.
func validID(id string) bool {
	u, err := uuid.Parse(id)
	return err == nil && u.String() == id
}
