// Package s3test connects tests to a real S3-compatible server (MinIO). Tests are skipped unless
// CONVERTFLOW_TEST_S3_ENDPOINT (host:port) and the key file variables
// CONVERTFLOW_TEST_S3_ACCESS_KEY_FILE / CONVERTFLOW_TEST_S3_SECRET_KEY_FILE are set; set
// CONVERTFLOW_TEST_S3_USE_SSL=true for a TLS endpoint. Each test gets its own bucket, which is
// emptied and removed afterwards.
package s3test

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/ShifuRain/convertflow/internal/blob"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Env is a configured test bucket.
type Env struct {
	Opts   blob.S3Options // ready for blob.NewS3 (ScratchDir is a fresh temp dir)
	Client *minio.Client  // admin client for assertions
}

// New skips the test without a configured endpoint, else creates a fresh bucket.
func New(t *testing.T) Env {
	t.Helper()
	ep := os.Getenv("CONVERTFLOW_TEST_S3_ENDPOINT")
	if ep == "" {
		t.Skip("CONVERTFLOW_TEST_S3_ENDPOINT not set")
	}
	read := func(k string) string {
		b, err := os.ReadFile(os.Getenv(k))
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		return strings.TrimSpace(string(b))
	}
	ak, sk := read("CONVERTFLOW_TEST_S3_ACCESS_KEY_FILE"), read("CONVERTFLOW_TEST_S3_SECRET_KEY_FILE")
	ssl, _ := strconv.ParseBool(os.Getenv("CONVERTFLOW_TEST_S3_USE_SSL"))
	c, err := minio.New(ep, &minio.Options{Creds: credentials.NewStaticV4(ak, sk, ""), Secure: ssl, Region: "us-east-1", BucketLookup: minio.BucketLookupPath})
	if err != nil {
		t.Fatal(err)
	}
	bucket := "cf-test-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
	ctx := context.Background()
	if err := c.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: "us-east-1"}); err != nil {
		t.Fatalf("creating bucket: %v", err)
	}
	t.Cleanup(func() {
		for o := range c.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
			if o.Err == nil {
				_ = c.RemoveObject(ctx, bucket, o.Key, minio.RemoveObjectOptions{})
			}
		}
		_ = c.RemoveBucket(ctx, bucket)
	})
	scheme := "http://"
	if ssl {
		scheme = "https://"
	}
	return Env{Client: c, Opts: blob.S3Options{Endpoint: ep, PublicEndpoint: scheme + ep, Bucket: bucket, Region: "us-east-1",
		UseSSL: ssl, PathStyle: true, AccessKey: ak, SecretKey: sk, ScratchDir: t.TempDir()}}
}

// Store builds the store for e.Opts.
func (e Env) Store(t *testing.T) *blob.S3 {
	t.Helper()
	s, err := blob.NewS3(e.Opts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
