// Package audit writes structured audit records (JSON, one per line).
package audit

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
)

type Logger struct{ l *slog.Logger }

func New(w io.Writer) *Logger { return &Logger{slog.New(slog.NewJSONHandler(w, nil))} }

// Event records an API-originated event with the client IP. Never pass file contents.
// Attributes resolved for the request (see Request) are added: "owner" (a short owner-key
// prefix, never a token or user id) and "client_ip" (the address behind a trusted proxy, when it
// differs from remote_ip).
func (a *Logger) Event(r *http.Request, event, jobID string, attrs ...any) {
	args := []any{"audit", true, "event", event, "job_id", jobID, "remote_ip", remoteIP(r)}
	if ref, _ := r.Context().Value(ctxKey{}).(*Request); ref != nil {
		if ref.ClientIP != "" && ref.ClientIP != remoteIP(r) {
			args = append(args, "client_ip", ref.ClientIP)
		}
		if ref.Owner != "" {
			args = append(args, "owner", ref.Owner)
		}
	}
	a.l.Info("audit", append(args, attrs...)...)
}

// Request holds audit attributes resolved while a request is handled. Handlers fill it in place
// (the owner is known only after the session lookup), so it is carried as a pointer.
type Request struct {
	Owner    string // short owner-key prefix
	ClientIP string
}

type ctxKey struct{}

// WithRequest attaches ref to ctx; Event reads it from the request's context.
func WithRequest(ctx context.Context, ref *Request) context.Context {
	return context.WithValue(ctx, ctxKey{}, ref)
}

// RequestFrom returns the attributes attached by WithRequest, or nil.
func RequestFrom(ctx context.Context) *Request {
	ref, _ := ctx.Value(ctxKey{}).(*Request)
	return ref
}

// System records an event with no request (worker, janitor).
func (a *Logger) System(event, jobID string, attrs ...any) {
	args := append([]any{"audit", true, "event", event, "job_id", jobID}, attrs...)
	a.l.Info("audit", args...)
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
