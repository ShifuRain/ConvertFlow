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
