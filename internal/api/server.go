// Package api implements the REST API, SSE stream and static UI hosting.
package api

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ShifuRain/convertflow/internal/audit"
	"github.com/ShifuRain/convertflow/internal/config"
	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/ShifuRain/convertflow/internal/ui"
)

type Deps struct {
	Store          *jobs.Store
	Cfg            config.Config
	Audit          *audit.Logger
	EventInterval  time.Duration // SSE poll interval; default 1s
	SSEHeartbeat   time.Duration // idle keepalive comment interval; default 15s
	SSEMaxLifetime time.Duration // hard cap on one SSE connection; default 10m

	// Test seams; nil selects the production implementation.
	OpenInput func(path string) (io.WriteCloser, error) // creates the exclusive input file
	FreeBytes func(dir string) (uint64, error)          // free space on the data volume
}

type Server struct {
	store *jobs.Store
	cfg   config.Config
	audit *audit.Logger
	every time.Duration

	sseSlots     chan struct{}
	sseHeartbeat time.Duration
	sseMaxLife   time.Duration

	uploadSlots chan struct{}
	openInput   func(string) (io.WriteCloser, error)
	freeBytes   func(string) (uint64, error)
}

const liveWindow = 30 * time.Second // a worker is "live" if it was seen this recently

func NewHandler(d Deps) http.Handler {
	s := &Server{store: d.Store, cfg: d.Cfg, audit: d.Audit, every: d.EventInterval}
	slots := d.Cfg.MaxConcurrentUploads
	if slots <= 0 {
		slots = 4
	}
	s.uploadSlots = make(chan struct{}, slots)
	sseCap := d.Cfg.MaxSSEStreams
	if sseCap <= 0 {
		sseCap = defaultMaxSSEStreams
	}
	s.openInput, s.freeBytes = d.OpenInput, d.FreeBytes
	if s.openInput == nil {
		s.openInput = DefaultOpenInput
	}
	if s.freeBytes == nil {
		s.freeBytes = diskFree
	}
	if s.every <= 0 {
		s.every = time.Second
	}
	s.sseSlots = make(chan struct{}, sseCap)
	s.sseHeartbeat, s.sseMaxLife = d.SSEHeartbeat, d.SSEMaxLifetime
	if s.sseHeartbeat <= 0 {
		s.sseHeartbeat = defaultSSEHeartbeat
	}
	if s.sseMaxLife <= 0 {
		s.sseMaxLife = defaultSSEMaxLifetime
	}
	return recoverer(securityHeaders(d.Cfg.TLSCertFile != "", noStoreAPI(s.rootMux())))
}

type route struct {
	method, pattern string
	h               http.HandlerFunc
}

func (s *Server) routeTable() []route {
	return []route{
		{"POST", "/api/v1/jobs", s.createJob},
		{"GET", "/api/v1/jobs", s.listJobs},
		{"GET", "/api/v1/jobs/{id}", s.getJob},
		{"DELETE", "/api/v1/jobs/{id}", s.deleteJob},
		{"GET", "/api/v1/jobs/{id}/events", s.events},
		{"GET", "/api/v1/jobs/{id}/output", s.output},
		{"GET", "/api/v1/presets", s.listPresets},
		{"GET", "/api/openapi.json", s.openapi},
		{"GET", "/healthz", s.healthz},
		{"GET", "/metrics", s.metrics},
	}
}

// routePatterns lists "METHOD pattern" for every registered route (used to keep the OpenAPI
// document in sync).
func routePatterns() []string {
	var out []string
	for _, r := range (&Server{}).routeTable() {
		out = append(out, r.method+" "+r.pattern)
	}
	return out
}

// rootMux serves the API routes and the UI. Everything under /api/ (plus /healthz and /metrics)
// is routed by apiRouter so that unknown paths and wrong methods answer with problem+json rather
// than falling through to the static file server.
func (s *Server) rootMux() http.Handler {
	routes := http.NewServeMux()
	methods := map[string]bool{}
	for _, r := range s.routeTable() {
		routes.HandleFunc(r.method+" "+r.pattern, r.h)
		methods[r.method] = true
		if r.method == "GET" {
			methods["HEAD"] = true
		}
	}
	probe := make([]string, 0, len(methods))
	for m := range methods {
		probe = append(probe, m)
	}
	sort.Strings(probe)
	apiRouter := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := routes.Handler(r); pattern != "" {
			routes.ServeHTTP(w, r)
			return
		}
		var allow []string
		for _, m := range probe {
			pr := r.Clone(r.Context())
			pr.Method = m
			if _, p := routes.Handler(pr); p != "" {
				allow = append(allow, m)
			}
		}
		if len(allow) > 0 {
			w.Header().Set("Allow", strings.Join(allow, ", "))
			WriteProblem(w, http.StatusMethodNotAllowed, "Method not allowed", "")
			return
		}
		WriteProblem(w, http.StatusNotFound, "Not found", "no such endpoint")
	})
	uiH := ui.Handler()
	mux := http.NewServeMux()
	mux.Handle("/api", apiRouter)
	mux.Handle("/api/", apiRouter)
	mux.Handle("/healthz", apiRouter)
	mux.Handle("/metrics", apiRouter)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			WriteProblem(w, http.StatusMethodNotAllowed, "Method not allowed", "")
			return
		}
		uiH.ServeHTTP(w, r)
	})
	return mux
}

// jobView is the public representation of a job (never exposes server paths).
type jobView struct {
	ID          string          `json:"id"`
	Status      jobs.Status     `json:"status"`
	Filename    string          `json:"filename"`
	Preset      string          `json:"preset"`
	Progress    float64         `json:"progress"`
	EncoderUsed string          `json:"encoder_used"`
	Probe       json.RawMessage `json:"probe,omitempty"`
	Error       string          `json:"error,omitempty"`
	Attempts    int             `json:"attempts"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	FinishedAt  *time.Time      `json:"finished_at,omitempty"`
	OutputURL   string          `json:"output_url,omitempty"`
}

func toView(j jobs.Job) jobView {
	v := jobView{ID: j.ID, Status: j.Status, Filename: j.Filename, Preset: j.Options.Preset,
		Progress: j.Progress, EncoderUsed: j.EncoderUsed, Probe: j.Probe, Error: j.Error,
		Attempts: j.Attempts, CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt, FinishedAt: j.FinishedAt}
	if j.Status == jobs.StatusDone {
		v.OutputURL = "/api/v1/jobs/" + j.ID + "/output"
	}
	return v
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeJob writes a job representation with the given status.
func (s *Server) writeJob(w http.ResponseWriter, status int, j *jobs.Job) {
	writeJSON(w, status, toView(*j))
}
