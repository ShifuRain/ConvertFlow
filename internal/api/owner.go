package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/ShifuRain/convertflow/internal/audit"
	"github.com/ShifuRain/convertflow/internal/config"
	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/ShifuRain/convertflow/internal/session"
)

// ident is who a request comes from, resolved once per /api/ request by identify.
type ident struct {
	owner     []byte // owner key; nil when the request has none (yet)
	token     string // the cookie token behind owner (cookie mode only); never logged
	conflict  bool   // cookie mode: several session cookies were sent
	clientIP  netip.Addr
	ipKey     []byte // client IP key (per-IP limits)
	auditInfo *audit.Request
}

type identKey struct{}

func identOf(r *http.Request) *ident {
	if id, _ := r.Context().Value(identKey{}).(*ident); id != nil {
		return id
	}
	return &ident{auditInfo: &audit.Request{}} // not routed through identify (tests of internals)
}

func (s *Server) sessionsOn() bool {
	return s.cfg.Sessions == config.SessionsCookie || s.cfg.Sessions == config.SessionsHeader
}

// identify resolves the caller of every /api/ request (except the static OpenAPI document): the
// client IP (behind a trusted proxy: from X-Forwarded-For), and with sessions on, the owner key
// from the session cookie or the trusted proxy's user header. It never creates a session; the
// handlers that need one call ensureOwner.
func (s *Server) identify(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api/openapi.json" {
			next.ServeHTTP(w, r)
			return
		}
		id := &ident{auditInfo: &audit.Request{}}
		id.clientIP = session.ClientIP(r, s.cfg.TrustedProxies)
		if id.clientIP.IsValid() {
			id.auditInfo.ClientIP = id.clientIP.String()
		}
		id.ipKey = session.IPKey(id.clientIP)
		switch s.cfg.Sessions {
		case config.SessionsCookie:
			if !s.cookieOwner(w, r, id) {
				return
			}
		case config.SessionsHeader:
			// Only a trusted proxy may assert a user; from anyone else the header is ignored.
			if session.FromTrustedProxy(r, s.cfg.TrustedProxies) {
				if v := r.Header.Values(s.cfg.SessionHeader); len(v) == 1 && session.ValidUser(v[0]) {
					id.owner = session.HeaderKey(v[0])
				}
			}
		}
		id.auditInfo.Owner = session.Prefix(id.owner)
		ctx := context.WithValue(audit.WithRequest(r.Context(), id.auditInfo), identKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// cookieOwner looks up the session cookie. An unknown, expired or malformed token is ignored
// (the request has no session); only tokens this server issued and still stores are accepted.
// It returns false after answering a database failure.
func (s *Server) cookieOwner(w http.ResponseWriter, r *http.Request, id *ident) bool {
	cs := r.CookiesNamed(session.CookieName)
	if len(cs) > 1 {
		// Several cookies of that name (for example one planted for a narrower path by a sibling
		// host): we cannot tell which is ours, so none is used and none is minted.
		id.conflict = true
		return true
	}
	if len(cs) == 0 {
		return true
	}
	key, ok := session.TokenKey(cs[0].Value)
	if !ok {
		return true
	}
	ctx, cancel := context.WithTimeout(r.Context(), readQueryTimeout)
	defer cancel()
	refreshed, err := s.store.TouchSession(ctx, key, s.cfg.SessionTTL)
	switch {
	case errors.Is(err, jobs.ErrNoSession):
		return true
	case err != nil:
		s.readFault(w, r, "session_lookup", err)
		return false
	}
	id.owner, id.token = key, cs[0].Value
	if refreshed {
		s.setSessionCookie(w, r, id.token) // renew the browser-side lifetime with the server-side one
	}
	return true
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: session.CookieName, Value: token, Path: session.CookiePath,
		MaxAge: int(s.cfg.SessionTTL / time.Second), HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil || session.ForwardedTLS(r, s.cfg.TrustedProxies),
	})
}

// scope returns the jobs the caller may see. With sessions off that is every job. In cookie mode a
// caller without a session gets a scope that matches nothing (an empty list, 404s). In header mode
// a request without a (trusted) user header is answered 401 and ok is false.
func (s *Server) scope(w http.ResponseWriter, r *http.Request) (jobs.Scope, bool) {
	if !s.sessionsOn() {
		return jobs.AllJobs(), true
	}
	id := identOf(r)
	if s.cfg.Sessions == config.SessionsHeader && id.owner == nil {
		s.unauthenticated(w, r)
		return jobs.Scope{}, false
	}
	return jobs.OwnedBy(id.owner), true
}

func (s *Server) unauthenticated(w http.ResponseWriter, r *http.Request) {
	s.audit.Event(r, "request_rejected", "", "status", http.StatusUnauthorized, "reason", "no_user_header")
	WriteProblem(w, http.StatusUnauthorized, "Unauthorized", "This server needs a user identity from its reverse proxy.")
}

// ensureOwner gives the caller an owner before a job is created: in cookie mode it creates a
// session (and sets the cookie) when the request has none. strict callers (job creation) get an
// error answer when that is impossible and false; non-strict callers (the page's first requests)
// carry on without a session. With sessions off it does nothing.
func (s *Server) ensureOwner(w http.ResponseWriter, r *http.Request, strict bool) bool {
	id := identOf(r)
	switch s.cfg.Sessions {
	case config.SessionsHeader:
		if id.owner == nil && strict {
			s.unauthenticated(w, r)
			return false
		}
		return true
	case config.SessionsCookie:
	default:
		return true
	}
	if id.owner != nil {
		return true
	}
	if id.conflict {
		if strict {
			s.reject(w, r, http.StatusBadRequest, "Conflicting session cookies",
				"Several session cookies were sent; clear this site's cookies and reload.", "session cookie conflict")
		}
		return !strict
	}
	token, key, err := session.NewToken()
	if err == nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), readQueryTimeout)
		err = s.store.CreateSession(ctx, key, session.MintKey(id.ipKey), s.cfg.Quota.SessionsPerHourPerIP)
		cancel()
	}
	var qe *jobs.QuotaError
	switch {
	case errors.As(err, &qe):
		if strict {
			s.quotaReject(w, r, qe)
			return false
		}
		s.audit.Event(r, "session_refused", "", "reason", qe.Kind)
		return true
	case err != nil:
		if strict {
			s.serverFault(w, r, "", "create_session", err)
			return false
		}
		slog.Error("creating a session failed", "err", err)
		return true
	}
	id.owner, id.token = key, token
	id.auditInfo.Owner = session.Prefix(key)
	s.setSessionCookie(w, r, token)
	s.audit.Event(r, "session_created", "")
	return true
}

// limits returns the configured quotas.
func (s *Server) limits() jobs.Limits {
	q := s.cfg.Quota
	return jobs.Limits{ActiveJobs: q.ActiveJobsPerOwner, QueuedBytes: q.QueuedBytesPerOwner,
		JobsPerHour: q.JobsPerHourPerOwner, ActiveJobsPerIP: q.ActiveJobsPerIP}
}

// stamp sets the ownership fields of a new job from the caller.
func (s *Server) stamp(r *http.Request, j *jobs.Job) {
	id := identOf(r)
	if s.sessionsOn() {
		j.Owner = id.owner
	}
	if s.cfg.Quota.ActiveJobsPerIP > 0 {
		j.ClientKey = id.ipKey
	}
}

// precheckQuota applies the quotas before any upload byte is read (announced is the expected
// input size, <= 0 if unknown). It is not atomic: CreateWithQuota re-checks under a lock.
func (s *Server) precheckQuota(w http.ResponseWriter, r *http.Request, announced int64) bool {
	var j jobs.Job
	s.stamp(r, &j)
	l := s.limits()
	if (j.Owner == nil || (l.ActiveJobs == 0 && l.QueuedBytes == 0 && l.JobsPerHour == 0)) && (j.ClientKey == nil || l.ActiveJobsPerIP == 0) {
		return true
	}
	u, err := s.store.QuotaUsage(r.Context(), j.Owner, j.ClientKey)
	if err != nil {
		s.serverFault(w, r, "", "quota_usage", err)
		return false
	}
	if qe := l.Check(u, max(announced, 0), j.Owner != nil, j.ClientKey != nil); qe != nil {
		s.quotaReject(w, r, qe)
		return false
	}
	return true
}

// quotaReject answers an exceeded quota: 413 when the single input is larger than the queued-bytes
// quota (retrying cannot help), otherwise 429 with Retry-After.
func (s *Server) quotaReject(w http.ResponseWriter, r *http.Request, qe *jobs.QuotaError) {
	if qe.Kind == jobs.QuotaInputTooLarge {
		s.reject(w, r, http.StatusRequestEntityTooLarge, "Upload too large", "The file is larger than your upload quota.", "quota "+qe.Kind)
		return
	}
	detail := map[string]string{
		jobs.QuotaActiveJobs:      "You have too many unfinished jobs; wait for one to finish or remove one.",
		jobs.QuotaQueuedBytes:     "Your unfinished jobs already use your upload volume quota; wait for one to finish or remove one.",
		jobs.QuotaJobsPerHour:     "You have created the maximum number of jobs for this hour.",
		jobs.QuotaActiveJobsPerIP: "Too many unfinished jobs from your network address; wait for one to finish.",
		jobs.QuotaSessionsPerHour: "Too many new sessions from your network address; retry later.",
	}[qe.Kind]
	secs := int64(max(qe.RetryAfter, time.Second) / time.Second)
	w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
	s.reject(w, r, http.StatusTooManyRequests, "Quota exceeded", detail, "quota "+qe.Kind)
}

// quotaView is the answer of GET /api/v1/quota.
type quotaView struct {
	Sessions string    `json:"sessions"`
	Owner    bool      `json:"owner"`
	Limits   quotaNums `json:"limits"`
	Usage    quotaNums `json:"usage"`
	Resets   *int64    `json:"jobs_per_hour_resets_in_seconds,omitempty"`
}

type quotaNums struct {
	ActiveJobs      int64 `json:"active_jobs"`
	QueuedBytes     int64 `json:"queued_bytes"`
	JobsPerHour     int64 `json:"jobs_per_hour"`
	ActiveJobsPerIP int64 `json:"active_jobs_per_ip"`
}

func (s *Server) quota(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Sessions == config.SessionsHeader && identOf(r).owner == nil {
		s.unauthenticated(w, r)
		return
	}
	s.ensureOwner(w, r, false)
	var j jobs.Job
	s.stamp(r, &j)
	ctx, cancel := context.WithTimeout(r.Context(), readQueryTimeout)
	defer cancel()
	u, err := s.store.QuotaUsage(ctx, j.Owner, j.ClientKey)
	if err != nil {
		s.readFault(w, r, "quota_usage", err)
		return
	}
	q := s.cfg.Quota
	v := quotaView{Sessions: s.cfg.Sessions, Owner: j.Owner != nil,
		Limits: quotaNums{int64(q.ActiveJobsPerOwner), q.QueuedBytesPerOwner, int64(q.JobsPerHourPerOwner), int64(q.ActiveJobsPerIP)},
		Usage:  quotaNums{int64(u.ActiveJobs), u.QueuedBytes, int64(u.JobsLastHour), int64(u.ActiveJobsPerIP)}}
	if v.Sessions == "" {
		v.Sessions = config.SessionsOff
	}
	if u.HourResetIn > 0 {
		secs := int64(u.HourResetIn / time.Second)
		v.Resets = &secs
	}
	writeJSON(w, http.StatusOK, v)
}
