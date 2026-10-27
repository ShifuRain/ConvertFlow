// Package jobs defines the job model and its Postgres-backed store.
package jobs

import "time"

type Status string

const (
	// StatusUploading: a presigned upload was handed out; the job is queued by
	// POST /jobs/{id}/complete once the object is verified.
	StatusUploading Status = "uploading"
	StatusQueued    Status = "queued"
	StatusProbing   Status = "probing"
	StatusRunning   Status = "running"
	StatusDone      Status = "done"
	StatusFailed    Status = "failed"
	StatusCanceled  Status = "canceled"
)

func (s Status) Terminal() bool {
	return s == StatusDone || s == StatusFailed || s == StatusCanceled
}

// Options are the only user-controllable transcode parameters (see Validate in options.go).
type Options struct {
	Preset        string `json:"preset"`
	Speed         *int   `json:"speed,omitempty"`
	CRF           *int   `json:"crf,omitempty"`
	MaxHeight     *int   `json:"max_height,omitempty"`
	AudioBitrateK *int   `json:"audio_bitrate_k,omitempty"`
}

type Job struct {
	ID              string
	Status          Status
	Filename        string
	InputPath       string
	OutputPath      string
	Options         Options
	Progress        float64
	EncoderUsed     string
	Probe           []byte
	Error           string
	Attempts        int
	WorkerID        string
	CancelRequested bool
	// DeclaredSize is the client-announced size of a presigned upload (0 if none).
	DeclaredSize int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
	FinishedAt   *time.Time
}
