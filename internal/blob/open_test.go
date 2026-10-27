package blob_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ShifuRain/convertflow/internal/blob"
	"github.com/ShifuRain/convertflow/internal/blob/s3test"
	"github.com/ShifuRain/convertflow/internal/config"
)

func TestOpenLocal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s, err := blob.Open(ctx, config.Config{Storage: config.StorageLocal, DataDir: dir}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.(*blob.LocalFS); !ok {
		t.Fatalf("got %T", s)
	}
	if _, ok := s.(blob.Presigner); ok {
		t.Fatal("the local store must not presign")
	}
	if st, err := os.Stat(filepath.Join(dir, "in")); err != nil || !st.IsDir() {
		t.Fatal("in/ not created")
	}
}

func s3Config(e s3test.Env) config.Config {
	return config.Config{Storage: config.StorageS3, S3: config.S3Config{Endpoint: e.Opts.Endpoint, PublicEndpoint: e.Opts.PublicEndpoint,
		Bucket: e.Opts.Bucket, Region: e.Opts.Region, UseSSL: e.Opts.UseSSL, PathStyle: true,
		AccessKey: config.Secret(e.Opts.AccessKey), SecretKey: config.Secret(e.Opts.SecretKey)}}
}

func TestOpenS3(t *testing.T) {
	e := s3test.New(t)
	cfg := s3Config(e)
	s, err := blob.Open(ctx, cfg, t.TempDir(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.(blob.Presigner); !ok {
		t.Fatalf("%T must presign", s)
	}
	cfg.S3.Bucket = "cf-test-missing-bucket-0000"
	if _, err := blob.Open(ctx, cfg, "", 0); !errors.Is(err, blob.ErrNoBucket) {
		t.Fatalf("missing bucket: %v", err)
	}
	cfg.S3.SecretKey = "wrong-secret-value"
	cfg.S3.Bucket = e.Opts.Bucket
	_, err = blob.Open(ctx, cfg, "", 0)
	if err == nil || strings.Contains(err.Error(), "wrong-secret-value") {
		t.Fatalf("rejected credentials must be a start error without the secret: %v", err)
	}
}
