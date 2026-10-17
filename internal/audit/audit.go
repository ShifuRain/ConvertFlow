// Package audit writes structured audit records (JSON, one per line).
package audit

import (
	"io"
	"log/slog"
	"net"
	"net/http"
)

type Logger struct{ l *slog.Logger }

func New(w io.Writer) *Logger { return &Logger{slog.New(slog.NewJSONHandler(w, nil))} }

// Event records an API-originated event with the client IP. Never pass file contents.
func (a *Logger) Event(r *http.Request, event, jobID string, attrs ...any) {
	args := append([]any{"audit", true, "event", event, "job_id", jobID, "remote_ip", remoteIP(r)}, attrs...)
	a.l.Info("audit", args...)
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
