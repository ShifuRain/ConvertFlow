// Package api implements the REST API, SSE stream and static UI hosting.
package api

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/ShifuRain/convertflow/internal/audit"
	"github.com/ShifuRain/convertflow/internal/config"
	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/ShifuRain/convertflow/internal/ui"
)

type Deps struct {
	Store         *jobs.Store
	Cfg           config.Config
	Audit         *audit.Logger
	EventInterval time.Duration // SSE poll interval; default 1s

	// Test seams; nil selects the production implementation.
	OpenInput func(path string) (io.WriteCloser, error) // creates the exclusive input file
	FreeBytes func(dir string) (uint64, error)          // free space on the data volume
}

type Server struct {
	store *jobs.Store
	cfg   config.Config
	audit *audit.Logger
	every time.Duration

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
	mux := http.NewServeMux()
	s.routes(mux)
	mux.Handle("/", ui.Handler())
	return recoverer(securityHeaders(d.Cfg.TLSCertFile != "", noStoreAPI(mux)))
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/jobs", s.createJob)
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
