// Package config parses ConvertFlow's environment-based configuration.
package config

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ShifuRain/convertflow/internal/hostcheck"
)

type Config struct {
	ListenAddr     string
	DatabaseURL    string
	DataDir        string
	Flavor         string // "lgpl" or "full"
	TLSCertFile    string
	TLSKeyFile     string
	VAAPIDevice    string
	MaxUploadBytes int64
	MaxPixels      int64
	MaxDurationSec float64
	MaxStreams     int
	MaxAttempts    int
	MaxOutputBytes int64         // worker kills a conversion whose output exceeds this
	JobTimeout     time.Duration // worker wall-clock limit for one FFmpeg run
	Retention      time.Duration
	StaleAfter     time.Duration
	MaxQueueAge    time.Duration // queued jobs older than this are failed by the janitor
	WorkerPoll     time.Duration
	// AllowedHosts extends the default Host allow-list (localhost and IP literals); a single "*"
	// disables the check. It defends against DNS rebinding.
	AllowedHosts []string

	// Upload resource bounds (the service has no authentication, so these cap what anyone on the
	// network can consume).
	MaxConcurrentUploads int   // simultaneous in-flight upload requests; excess get 503
	MaxQueuedJobs        int   // queued+probing+running jobs; excess uploads get 503
	MinFreeBytes         int64 // free space required on the data volume; below it uploads get 507
	MaxSSEStreams        int   // simultaneous progress streams; excess get 503
}

func Load(getenv func(string) string, readFile func(string) ([]byte, error)) (Config, error) {
	var firstErr error
	fail := func(format string, a ...any) {
		if firstErr == nil {
			firstErr = fmt.Errorf(format, a...)
		}
	}
	str := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	num := func(key string, def int64) int64 {
		v := getenv(key)
		if v == "" {
			return def
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			fail("%s must be a positive integer, got %q", key, v)
			return def
		}
		return n
	}
	dur := func(key string, def time.Duration) time.Duration {
		v := getenv(key)
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			fail("%s must be a positive duration, got %q", key, v)
			return def
		}
		return d
	}

	c := Config{
		ListenAddr:     str("CONVERTFLOW_LISTEN", "127.0.0.1:8080"),
		DataDir:        str("CONVERTFLOW_DATA_DIR", "/data"),
		Flavor:         str("CONVERTFLOW_FLAVOR", "lgpl"),
		TLSCertFile:    getenv("CONVERTFLOW_TLS_CERT_FILE"),
		TLSKeyFile:     getenv("CONVERTFLOW_TLS_KEY_FILE"),
		VAAPIDevice:    str("CONVERTFLOW_VAAPI_DEVICE", "/dev/dri/renderD128"),
		MaxUploadBytes: num("CONVERTFLOW_MAX_UPLOAD_BYTES", 8<<30),
		MaxPixels:      num("CONVERTFLOW_MAX_PIXELS", 8192*4320),
		MaxDurationSec: float64(num("CONVERTFLOW_MAX_DURATION_SEC", 6*3600)),
		MaxStreams:     int(num("CONVERTFLOW_MAX_STREAMS", 16)),
		MaxAttempts:    int(num("CONVERTFLOW_MAX_ATTEMPTS", 3)),
		MaxOutputBytes: num("CONVERTFLOW_MAX_OUTPUT_BYTES", 16<<30),
		JobTimeout:     dur("CONVERTFLOW_JOB_TIMEOUT", 4*time.Hour),
		Retention:      dur("CONVERTFLOW_RETENTION", 24*time.Hour),
		StaleAfter:     dur("CONVERTFLOW_STALE_AFTER", 60*time.Second),
		MaxQueueAge:    dur("CONVERTFLOW_MAX_QUEUE_AGE", 24*time.Hour),
		WorkerPoll:     dur("CONVERTFLOW_WORKER_POLL", 2*time.Second),
	}
	c.MaxConcurrentUploads = int(num("CONVERTFLOW_MAX_CONCURRENT_UPLOADS", 4))
	c.MaxQueuedJobs = int(num("CONVERTFLOW_MAX_QUEUED_JOBS", 100))
	c.MaxSSEStreams = int(num("CONVERTFLOW_MAX_SSE_STREAMS", 64))
	// Default: room for one maximum-size upload plus a similarly sized output.
	c.MinFreeBytes = num("CONVERTFLOW_MIN_FREE_BYTES", 2*c.MaxUploadBytes)
	if v := getenv("CONVERTFLOW_ALLOWED_HOSTS"); v != "" {
		for _, h := range strings.Split(v, ",") {
			if h = strings.TrimSpace(h); h != "" {
				c.AllowedHosts = append(c.AllowedHosts, h)
			}
		}
		if _, err := hostcheck.Parse(c.AllowedHosts); err != nil {
			fail("CONVERTFLOW_ALLOWED_HOSTS: %v", err)
		}
	}
	if c.Flavor != "lgpl" && c.Flavor != "full" {
		fail("CONVERTFLOW_FLAVOR must be \"lgpl\" or \"full\", got %q", c.Flavor)
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		fail("CONVERTFLOW_TLS_CERT_FILE and CONVERTFLOW_TLS_KEY_FILE must be set together")
	}
	c.DatabaseURL = getenv("CONVERTFLOW_DATABASE_URL")
	if f := getenv("CONVERTFLOW_DATABASE_URL_FILE"); f != "" {
		b, err := readFile(f)
		if err != nil {
			fail("reading CONVERTFLOW_DATABASE_URL_FILE: %v", err)
		} else {
			c.DatabaseURL = strings.TrimSpace(string(b))
		}
	}
	if c.DatabaseURL == "" {
		fail("CONVERTFLOW_DATABASE_URL or CONVERTFLOW_DATABASE_URL_FILE is required")
	}
	return c, firstErr
}

// Absolutize makes every filesystem path of the configuration absolute, so later working-directory
// changes or relative comparisons (the janitor's inside-the-data-dir check) cannot be confused.
func (c *Config) Absolutize() error {
	for _, p := range []*string{&c.DataDir, &c.TLSCertFile, &c.TLSKeyFile} {
		if *p == "" {
			continue
		}
		abs, err := filepath.Abs(*p)
		if err != nil {
			return fmt.Errorf("resolving %q: %w", *p, err)
		}
		*p = abs
	}
	return nil
}
