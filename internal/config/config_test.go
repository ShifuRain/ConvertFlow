package config_test

import (
	"errors"
	"testing"
	"time"

	"github.com/ShifuRain/convertflow/internal/config"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
func noFile(string) ([]byte, error)               { return nil, errors.New("no file") }

func TestDefaults(t *testing.T) {
	c, err := config.Load(env(map[string]string{"CONVERTFLOW_DATABASE_URL": "postgres://x"}), noFile)
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != "127.0.0.1:8080" || c.DataDir != "/data" || c.Flavor != "lgpl" {
		t.Fatalf("bad defaults: %+v", c)
	}
	if c.Retention != 24*time.Hour || c.MaxAttempts != 3 || c.MaxStreams != 16 {
		t.Fatalf("bad defaults: %+v", c)
	}
	if c.MaxUploadBytes != 8<<30 || c.MaxPixels != 8192*4320 || c.MaxDurationSec != 6*3600 {
		t.Fatalf("bad limits: %+v", c)
	}
}

func TestDatabaseURLFile(t *testing.T) {
	read := func(p string) ([]byte, error) {
		if p != "/run/secrets/u" {
			return nil, errors.New("wrong path")
		}
		return []byte("postgres://from-file\n"), nil
	}
	c, err := config.Load(env(map[string]string{"CONVERTFLOW_DATABASE_URL_FILE": "/run/secrets/u"}), read)
	if err != nil || c.DatabaseURL != "postgres://from-file" {
		t.Fatalf("got %q, %v", c.DatabaseURL, err)
	}
}

func TestInvalidValues(t *testing.T) {
	base := map[string]string{"CONVERTFLOW_DATABASE_URL": "postgres://x"}
	cases := map[string]map[string]string{
		"missing db": {},
		"bad flavor": {"CONVERTFLOW_FLAVOR": "gpl"},
		"bad size":   {"CONVERTFLOW_MAX_UPLOAD_BYTES": "abc"},
		"neg size":   {"CONVERTFLOW_MAX_UPLOAD_BYTES": "-5"},
		"bad retain": {"CONVERTFLOW_RETENTION": "tomorrow"},
	}
	for name, extra := range cases {
		m := map[string]string{}
		if name != "missing db" {
			for k, v := range base {
				m[k] = v
			}
		}
		for k, v := range extra {
			m[k] = v
		}
		if _, err := config.Load(env(m), noFile); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestRetentionOverride(t *testing.T) {
	c, err := config.Load(env(map[string]string{"CONVERTFLOW_DATABASE_URL": "x", "CONVERTFLOW_RETENTION": "36h"}), noFile)
	if err != nil || c.Retention != 36*time.Hour {
		t.Fatalf("got %v, %v", c.Retention, err)
	}
}

func TestWorkerBudgets(t *testing.T) {
	base := map[string]string{"CONVERTFLOW_DATABASE_URL": "postgres://x"}
	c, err := config.Load(env(base), noFile)
	if err != nil || c.JobTimeout != 4*time.Hour || c.MaxOutputBytes != 16<<30 {
		t.Fatalf("bad defaults: %+v %v", c, err)
	}
	base["CONVERTFLOW_JOB_TIMEOUT"] = "90m"
	base["CONVERTFLOW_MAX_OUTPUT_BYTES"] = "1000"
	c, err = config.Load(env(base), noFile)
	if err != nil || c.JobTimeout != 90*time.Minute || c.MaxOutputBytes != 1000 {
		t.Fatalf("bad overrides: %+v %v", c, err)
	}
	base["CONVERTFLOW_JOB_TIMEOUT"] = "-1s"
	if _, err = config.Load(env(base), noFile); err == nil {
		t.Fatal("negative timeout must be rejected")
	}
}

func TestUploadResourceBounds(t *testing.T) {
	base := map[string]string{"CONVERTFLOW_DATABASE_URL": "postgres://x"}
	c, err := config.Load(env(base), noFile)
	if err != nil || c.MaxConcurrentUploads != 4 || c.MaxQueuedJobs != 100 || c.MinFreeBytes != 2*c.MaxUploadBytes {
		t.Fatalf("bad defaults: %+v %v", c, err)
	}
	// The free-space default follows a custom upload limit unless overridden.
	base["CONVERTFLOW_MAX_UPLOAD_BYTES"] = "1000"
	c, _ = config.Load(env(base), noFile)
	if c.MinFreeBytes != 2000 {
		t.Fatalf("default MinFreeBytes should be 2*MaxUploadBytes, got %d", c.MinFreeBytes)
	}
	base["CONVERTFLOW_MAX_CONCURRENT_UPLOADS"] = "9"
	base["CONVERTFLOW_MAX_QUEUED_JOBS"] = "7"
	base["CONVERTFLOW_MIN_FREE_BYTES"] = "555"
	c, err = config.Load(env(base), noFile)
	if err != nil || c.MaxConcurrentUploads != 9 || c.MaxQueuedJobs != 7 || c.MinFreeBytes != 555 {
		t.Fatalf("bad overrides: %+v %v", c, err)
	}
	for _, k := range []string{"CONVERTFLOW_MAX_CONCURRENT_UPLOADS", "CONVERTFLOW_MAX_QUEUED_JOBS", "CONVERTFLOW_MIN_FREE_BYTES"} {
		b := map[string]string{"CONVERTFLOW_DATABASE_URL": "postgres://x", k: "0"}
		if _, err := config.Load(env(b), noFile); err == nil {
			t.Errorf("%s=0 must be rejected", k)
		}
	}
}

func TestSSEStreamCap(t *testing.T) {
	base := map[string]string{"CONVERTFLOW_DATABASE_URL": "postgres://x"}
	c, err := config.Load(env(base), noFile)
	if err != nil || c.MaxSSEStreams != 64 {
		t.Fatalf("default: %+v %v", c, err)
	}
	base["CONVERTFLOW_MAX_SSE_STREAMS"] = "5"
	if c, err = config.Load(env(base), noFile); err != nil || c.MaxSSEStreams != 5 {
		t.Fatalf("override: %+v %v", c, err)
	}
	base["CONVERTFLOW_MAX_SSE_STREAMS"] = "0"
	if _, err = config.Load(env(base), noFile); err == nil {
		t.Fatal("0 must be rejected")
	}
}

func TestMaxQueueAge(t *testing.T) {
	base := map[string]string{"CONVERTFLOW_DATABASE_URL": "x"}
	if c, err := config.Load(env(base), noFile); err != nil || c.MaxQueueAge != 24*time.Hour {
		t.Fatalf("default: %v %v", c.MaxQueueAge, err)
	}
	base["CONVERTFLOW_MAX_QUEUE_AGE"] = "90m"
	if c, err := config.Load(env(base), noFile); err != nil || c.MaxQueueAge != 90*time.Minute {
		t.Fatalf("override: %v %v", c.MaxQueueAge, err)
	}
	for _, bad := range []string{"0", "0s", "-1h", "soon"} {
		base["CONVERTFLOW_MAX_QUEUE_AGE"] = bad
		if _, err := config.Load(env(base), noFile); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}
