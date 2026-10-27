package config_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
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

func TestAllowedHosts(t *testing.T) {
	base := map[string]string{"CONVERTFLOW_DATABASE_URL": "postgres://x"}
	c, err := config.Load(env(base), noFile)
	if err != nil || len(c.AllowedHosts) != 0 {
		t.Fatalf("default: %v %v", c.AllowedHosts, err)
	}
	base["CONVERTFLOW_ALLOWED_HOSTS"] = " convert.example.org , files.example:8443,[::1] "
	c, err = config.Load(env(base), noFile)
	if err != nil || len(c.AllowedHosts) != 3 || c.AllowedHosts[1] != "files.example:8443" {
		t.Fatalf("list: %v %v", c.AllowedHosts, err)
	}
	base["CONVERTFLOW_ALLOWED_HOSTS"] = "*"
	if c, err = config.Load(env(base), noFile); err != nil || len(c.AllowedHosts) != 1 {
		t.Fatalf("star: %v %v", c.AllowedHosts, err)
	}
	for _, bad := range []string{"http://x.example", "x.example/path", "a b", "x@y", "*,x.example", "x:port", "::1"} {
		base["CONVERTFLOW_ALLOWED_HOSTS"] = bad
		if _, err := config.Load(env(base), noFile); err == nil || !strings.Contains(err.Error(), "CONVERTFLOW_ALLOWED_HOSTS") {
			t.Errorf("%q: want error naming the variable, got %v", bad, err)
		}
	}
}

func TestAbsolutize(t *testing.T) {
	c, err := config.Load(env(map[string]string{"CONVERTFLOW_DATABASE_URL": "x", "CONVERTFLOW_DATA_DIR": "rel/data",
		"CONVERTFLOW_TLS_CERT_FILE": "c.pem", "CONVERTFLOW_TLS_KEY_FILE": "k.pem"}), noFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Absolutize(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{c.DataDir, c.TLSCertFile, c.TLSKeyFile} {
		if !filepath.IsAbs(p) {
			t.Errorf("not absolute: %q", p)
		}
	}
	if !strings.HasSuffix(c.DataDir, "/rel/data") {
		t.Errorf("DataDir = %q", c.DataDir)
	}
	var empty config.Config
	if err := empty.Absolutize(); err != nil || empty.TLSCertFile != "" {
		t.Errorf("unset paths must stay unset: %+v %v", empty, err)
	}
}

func s3Env(extra map[string]string) map[string]string {
	m := map[string]string{
		"CONVERTFLOW_DATABASE_URL":       "postgres://x",
		"CONVERTFLOW_STORAGE":            "s3",
		"CONVERTFLOW_S3_ENDPOINT":        "minio:9000",
		"CONVERTFLOW_S3_BUCKET":          "convertflow",
		"CONVERTFLOW_S3_ACCESS_KEY_FILE": "/run/secrets/ak",
		"CONVERTFLOW_S3_SECRET_KEY_FILE": "/run/secrets/sk",
	}
	for k, v := range extra {
		if v == "" {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	return m
}

func keyFiles(p string) ([]byte, error) {
	switch p {
	case "/run/secrets/ak":
		return []byte("AKIDEXAMPLE\n"), nil
	case "/run/secrets/sk":
		return []byte("very-secret-key\n"), nil
	case "/run/secrets/empty":
		return []byte("  \n"), nil
	}
	return nil, errors.New("no such file")
}

func TestStorageDefaultsToLocal(t *testing.T) {
	c, err := config.Load(env(map[string]string{"CONVERTFLOW_DATABASE_URL": "postgres://x"}), noFile)
	if err != nil || c.Storage != config.StorageLocal || c.S3 != (config.S3Config{}) {
		t.Fatalf("got %q %+v %v", c.Storage, c.S3, err)
	}
	// S3 variables are ignored (not validated) in local mode, except the refused plain secrets.
	c, err = config.Load(env(map[string]string{"CONVERTFLOW_DATABASE_URL": "x", "CONVERTFLOW_S3_BUCKET": "Not Valid"}), noFile)
	if err != nil || c.S3.Bucket != "" {
		t.Fatalf("local mode must ignore S3 settings: %+v %v", c.S3, err)
	}
}

func TestS3Defaults(t *testing.T) {
	c, err := config.Load(env(s3Env(nil)), keyFiles)
	if err != nil {
		t.Fatal(err)
	}
	s := c.S3
	if c.Storage != "s3" || s.Endpoint != "minio:9000" || s.PublicEndpoint != "https://minio:9000" || s.Bucket != "convertflow" ||
		s.Region != "us-east-1" || !s.UseSSL || !s.PathStyle || s.PresignTTL != 15*time.Minute {
		t.Fatalf("bad defaults: %+v", s)
	}
	if s.AccessKey.Value() != "AKIDEXAMPLE" || s.SecretKey.Value() != "very-secret-key" {
		t.Fatal("credentials not read from the files (trimmed)")
	}
}

func TestS3Overrides(t *testing.T) {
	c, err := config.Load(env(s3Env(map[string]string{
		"CONVERTFLOW_S3_PUBLIC_ENDPOINT": "https://files.example.org/",
		"CONVERTFLOW_S3_REGION":          "eu-central-1",
		"CONVERTFLOW_S3_USE_SSL":         "false",
		"CONVERTFLOW_S3_PATH_STYLE":      "false",
		"CONVERTFLOW_S3_PRESIGN_TTL":     "1h",
	})), keyFiles)
	if err != nil {
		t.Fatal(err)
	}
	s := c.S3
	if s.PublicEndpoint != "https://files.example.org" || s.Region != "eu-central-1" || s.UseSSL || s.PathStyle || s.PresignTTL != time.Hour {
		t.Fatalf("bad overrides: %+v", s)
	}
	// Without TLS the default public endpoint is http.
	c, err = config.Load(env(s3Env(map[string]string{"CONVERTFLOW_S3_USE_SSL": "0"})), keyFiles)
	if err != nil || c.S3.PublicEndpoint != "http://minio:9000" {
		t.Fatalf("public default without TLS: %q %v", c.S3.PublicEndpoint, err)
	}
}

func TestS3InvalidValues(t *testing.T) {
	cases := map[string]map[string]string{
		"unknown storage":     {"CONVERTFLOW_STORAGE": "gcs"},
		"upper storage":       {"CONVERTFLOW_STORAGE": "S3"},
		"missing endpoint":    {"CONVERTFLOW_S3_ENDPOINT": ""},
		"endpoint scheme":     {"CONVERTFLOW_S3_ENDPOINT": "http://minio:9000"},
		"endpoint path":       {"CONVERTFLOW_S3_ENDPOINT": "minio:9000/x"},
		"endpoint userinfo":   {"CONVERTFLOW_S3_ENDPOINT": "u@minio:9000"},
		"endpoint bad port":   {"CONVERTFLOW_S3_ENDPOINT": "minio:99999"},
		"endpoint no host":    {"CONVERTFLOW_S3_ENDPOINT": ":9000"},
		"public no scheme":    {"CONVERTFLOW_S3_PUBLIC_ENDPOINT": "files.example.org"},
		"public ftp":          {"CONVERTFLOW_S3_PUBLIC_ENDPOINT": "ftp://files.example.org"},
		"public path":         {"CONVERTFLOW_S3_PUBLIC_ENDPOINT": "https://files.example.org/s3"},
		"public query":        {"CONVERTFLOW_S3_PUBLIC_ENDPOINT": "https://files.example.org?x=1"},
		"public userinfo":     {"CONVERTFLOW_S3_PUBLIC_ENDPOINT": "https://u:p@files.example.org"},
		"missing bucket":      {"CONVERTFLOW_S3_BUCKET": ""},
		"bad bucket":          {"CONVERTFLOW_S3_BUCKET": "Upper_Case"},
		"short bucket":        {"CONVERTFLOW_S3_BUCKET": "ab"},
		"bucket dots":         {"CONVERTFLOW_S3_BUCKET": "a..b"},
		"bad region":          {"CONVERTFLOW_S3_REGION": "eu/1"},
		"bad ssl":             {"CONVERTFLOW_S3_USE_SSL": "maybe"},
		"bad path style":      {"CONVERTFLOW_S3_PATH_STYLE": "yes please"},
		"ttl too long":        {"CONVERTFLOW_S3_PRESIGN_TTL": "61m"},
		"ttl zero":            {"CONVERTFLOW_S3_PRESIGN_TTL": "0s"},
		"ttl sub-second":      {"CONVERTFLOW_S3_PRESIGN_TTL": "500ms"},
		"ttl garbage":         {"CONVERTFLOW_S3_PRESIGN_TTL": "soon"},
		"missing access file": {"CONVERTFLOW_S3_ACCESS_KEY_FILE": ""},
		"missing secret file": {"CONVERTFLOW_S3_SECRET_KEY_FILE": ""},
		"unreadable secret":   {"CONVERTFLOW_S3_SECRET_KEY_FILE": "/nope"},
		"empty secret":        {"CONVERTFLOW_S3_SECRET_KEY_FILE": "/run/secrets/empty"},
		"plain secret env":    {"CONVERTFLOW_S3_SECRET_KEY": "inline"},
		"plain access env":    {"CONVERTFLOW_S3_ACCESS_KEY": "inline"},
	}
	for name, extra := range cases {
		if _, err := config.Load(env(s3Env(extra)), keyFiles); err == nil {
			t.Errorf("%s: expected error", name)
		} else if strings.Contains(err.Error(), "very-secret-key") || strings.Contains(err.Error(), "inline") {
			t.Errorf("%s: error leaks a secret: %v", name, err)
		}
	}
	// A plain secret is refused even in local mode, so it is never silently ignored.
	if _, err := config.Load(env(map[string]string{"CONVERTFLOW_DATABASE_URL": "x", "CONVERTFLOW_S3_SECRET_KEY": "inline"}), noFile); err == nil {
		t.Error("plain CONVERTFLOW_S3_SECRET_KEY must be refused in local mode too")
	}
}

func TestSecretsAreRedactedWhenPrinted(t *testing.T) {
	c, err := config.Load(env(s3Env(nil)), keyFiles)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"%v", "%+v", "%#v", "%s"} {
		out := fmt.Sprintf(f, c)
		if strings.Contains(out, "very-secret-key") || strings.Contains(out, "AKIDEXAMPLE") {
			t.Errorf("%s leaks a credential: %s", f, out)
		}
	}
}

func TestWorkerModeAndStallDefaults(t *testing.T) {
	c, err := config.Load(env(map[string]string{"CONVERTFLOW_DATABASE_URL": "postgres://x"}), noFile)
	if err != nil {
		t.Fatal(err)
	}
	if c.WorkerOnce || c.WorkerIdleExit != 0 || c.WorkersOnDemand {
		t.Fatalf("always-on must be the default: %+v", c)
	}
	if c.JobStallTimeout != 10*time.Minute || c.WorkerShutdownTimeout != 10*time.Second {
		t.Fatalf("bad defaults: stall %s shutdown %s", c.JobStallTimeout, c.WorkerShutdownTimeout)
	}
}

func TestWorkerModeAndStallOverrides(t *testing.T) {
	c, err := config.Load(env(map[string]string{
		"CONVERTFLOW_DATABASE_URL":            "postgres://x",
		"CONVERTFLOW_WORKER_ONCE":             "true",
		"CONVERTFLOW_WORKER_IDLE_EXIT":        "45s",
		"CONVERTFLOW_WORKER_SHUTDOWN_TIMEOUT": "20s",
		"CONVERTFLOW_JOB_STALL_TIMEOUT":       "0",
		"CONVERTFLOW_WORKERS_ON_DEMAND":       "true",
	}), noFile)
	if err != nil {
		t.Fatal(err)
	}
	if !c.WorkerOnce || c.WorkerIdleExit != 45*time.Second || c.WorkerShutdownTimeout != 20*time.Second ||
		c.JobStallTimeout != 0 || !c.WorkersOnDemand {
		t.Fatalf("overrides not applied: %+v", c)
	}
	c, err = config.Load(env(map[string]string{"CONVERTFLOW_DATABASE_URL": "postgres://x", "CONVERTFLOW_JOB_STALL_TIMEOUT": "15m"}), noFile)
	if err != nil || c.JobStallTimeout != 15*time.Minute {
		t.Fatalf("got %s, %v", c.JobStallTimeout, err)
	}
}

func TestWorkerModeAndStallInvalid(t *testing.T) {
	for name, kv := range map[string][2]string{
		"once not bool":      {"CONVERTFLOW_WORKER_ONCE", "yes please"},
		"idle zero":          {"CONVERTFLOW_WORKER_IDLE_EXIT", "0"},
		"idle negative":      {"CONVERTFLOW_WORKER_IDLE_EXIT", "-5s"},
		"idle no unit":       {"CONVERTFLOW_WORKER_IDLE_EXIT", "30"},
		"stall negative":     {"CONVERTFLOW_JOB_STALL_TIMEOUT", "-1m"},
		"stall garbage":      {"CONVERTFLOW_JOB_STALL_TIMEOUT", "soon"},
		"stall too short":    {"CONVERTFLOW_JOB_STALL_TIMEOUT", "2s"},
		"shutdown zero":      {"CONVERTFLOW_WORKER_SHUTDOWN_TIMEOUT", "0s"},
		"shutdown too long":  {"CONVERTFLOW_WORKER_SHUTDOWN_TIMEOUT", "10m"},
		"on demand not bool": {"CONVERTFLOW_WORKERS_ON_DEMAND", "sometimes"},
	} {
		_, err := config.Load(env(map[string]string{"CONVERTFLOW_DATABASE_URL": "postgres://x", kv[0]: kv[1]}), noFile)
		if err == nil || !strings.Contains(err.Error(), kv[0]) {
			t.Errorf("%s: want an error naming %s, got %v", name, kv[0], err)
		}
	}
}

func TestWorkersOnDemand(t *testing.T) {
	for v, want := range map[string]bool{"": false, "false": false, "true": true} {
		c, err := config.Load(env(map[string]string{"CONVERTFLOW_DATABASE_URL": "postgres://x", "CONVERTFLOW_WORKERS_ON_DEMAND": v}), noFile)
		if err != nil || c.WorkersOnDemand != want {
			t.Errorf("%q: got %v %v", v, c.WorkersOnDemand, err)
		}
	}
	if _, err := config.Load(env(map[string]string{"CONVERTFLOW_DATABASE_URL": "postgres://x", "CONVERTFLOW_WORKERS_ON_DEMAND": "sometimes"}), noFile); err == nil {
		t.Error("invalid value accepted")
	}
}
