package blob

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ShifuRain/convertflow/internal/config"
	"github.com/minio/minio-go/v7"
)

// Open returns the store selected by cfg.Storage. For local storage it creates DataDir/in and
// DataDir/out. For S3 it checks that the bucket exists: a missing bucket is a start error, an
// unreachable endpoint only a warning (the store may come up later; operations then fail and are
// retried). Credentials the store rejects are a start error; credentials that merely may not see
// the bucket (least privilege, AccessDenied) are accepted with a warning. scratchDir is where the S3 store keeps local output files before upload (worker);
// maxInput caps a downloaded input (worker; 0 = no cap).
func Open(ctx context.Context, cfg config.Config, scratchDir string, maxInput int64) (Store, error) {
	if cfg.Storage != config.StorageS3 {
		return NewLocalFS(cfg.DataDir)
	}
	s, err := NewS3(S3Options{
		Endpoint: cfg.S3.Endpoint, PublicEndpoint: cfg.S3.PublicEndpoint, Bucket: cfg.S3.Bucket, Region: cfg.S3.Region,
		UseSSL: cfg.S3.UseSSL, PathStyle: cfg.S3.PathStyle, AccessKey: cfg.S3.AccessKey.Value(), SecretKey: cfg.S3.SecretKey.Value(),
		ScratchDir: scratchDir, MaxInputBytes: maxInput,
	})
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err = s.Check(cctx)
	code := minio.ToErrorResponse(err).Code
	switch {
	case err == nil:
	case errors.Is(err, ErrNoBucket):
		return nil, fmt.Errorf("CONVERTFLOW_S3_BUCKET %q: %w", cfg.S3.Bucket, err)
	case code == "InvalidAccessKeyId" || code == "SignatureDoesNotMatch":
		return nil, fmt.Errorf("object store refused the credentials (%s)", code)
	case code == "AccessDenied":
		// Least-privilege credentials (object access only) may not see the bucket itself.
		slog.Warn("object store credentials cannot check the bucket; continuing")
	default:
		slog.Warn("object store not reachable at startup; continuing", "err", err)
	}
	return s, nil
}
