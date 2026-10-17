package api

import (
	"log/slog"
	"net/http"
	"net/url"
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
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
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

// crossOriginAllowed decides whether a state-changing request may proceed. Browsers always send
// Sec-Fetch-Site (and Origin on cross-origin POST/DELETE); clients that send neither (curl,
// scripts, automation) are not subject to cross-origin attacks and stay allowed. The Host header
// is compared, never X-Forwarded-Host (which an attacker page cannot set but a proxy chain might
// have left attacker-influenced).
func crossOriginAllowed(r *http.Request) bool {
	if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" {
		return sfs == "same-origin" || sfs == "none"
	}
	vals := r.Header.Values("Origin")
	if len(vals) == 0 {
		return true
	}
	if len(vals) > 1 {
		return false
	}
	u, err := url.Parse(vals[0])
	if err != nil || u.Host == "" || r.Host == "" {
		return false // includes the opaque origin "null"
	}
	return strings.EqualFold(u.Host, r.Host)
}

func isStateChanging(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// crossOriginGuard rejects state-changing /api/ requests that a browser marks as cross-origin,
// before the handler (and so any body read, file write or database access) runs.
func (s *Server) crossOriginGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isStateChanging(r.Method) && strings.HasPrefix(r.URL.Path, "/api") && !crossOriginAllowed(r) {
			s.audit.Event(r, "request_rejected", "", "status", http.StatusForbidden, "reason", "cross_origin")
			w.Header().Set("Connection", "close")
			WriteProblem(w, http.StatusForbidden, "Forbidden", "Cross-origin requests are not allowed.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostGuard answers 421 to every request (API, SSE, downloads, UI, probes) whose Host header is not
// in the allow-list: the DNS rebinding defense. The audit record carries a fixed reason only,
// never the attacker-controlled Host value.
func (s *Server) hostGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hosts.Allowed(r.Host) {
			s.audit.Event(r, "request_rejected", "", "status", http.StatusMisdirectedRequest, "reason", "host_not_allowed")
			w.Header().Set("Connection", "close")
			WriteProblem(w, http.StatusMisdirectedRequest, "Misdirected request", "The Host header is not allowed.")
			return
		}
		next.ServeHTTP(w, r)
	})
}
