package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3 stores inputs as <bucket>/in/<id> and outputs as <bucket>/out/<id>.<attempt>.<ext>; the
// locators are the object keys. It implements Store and Presigner.
//
// Two clients are kept: one for the endpoint the api and worker reach (all data operations), and
// one for the public endpoint browsers reach, used only to sign presigned URLs (the host is part
// of a V4 signature, so a URL must be signed for the host the browser will contact).
type S3 struct {
	c, pub   *minio.Client
	bucket   string
	scratch  string
	maxInput int64
	origin   string // origin of presigned upload URLs (see UploadOrigin)
}

// S3Options configures an S3 store.
type S3Options struct {
	Endpoint       string // host:port reached by this process
	PublicEndpoint string // scheme://host[:port] reached by browsers (presigned URLs)
	Bucket         string
	Region         string
	UseSSL         bool
	PathStyle      bool
	AccessKey      string
	SecretKey      string
	// ScratchDir is where NewOutput places local output files before commit uploads them
	// (default os.TempDir()). The worker passes its private scratch directory.
	ScratchDir string
	// MaxInputBytes makes LocalInput refuse (ErrTooLarge) an input larger than this, so an object
	// replaced after the API checked its size cannot reach FFmpeg. Zero disables the check.
	MaxInputBytes int64
	// Transport overrides the HTTP transport (tests). Nil uses minio-go's default.
	Transport http.RoundTripper
}

var _ Store = (*S3)(nil)
var _ Presigner = (*S3)(nil)

// ErrTooLarge is returned by LocalInput when an input exceeds S3Options.MaxInputBytes.
var ErrTooLarge = errors.New("blob: input exceeds the size limit")

// s3PartSize is the multipart part size for uploads of unknown length (and of outputs). It is
// also the memory buffered per concurrent upload. With S3's limit of 10000 parts it caps a
// streamed object at about 156 GiB.
const s3PartSize = 16 << 20

// Object keys. LocatorPattern is the general form; every operation additionally requires the
// exact form of its namespace (in/<id>, out/<id>.<attempt>.<ext>).
var (
	LocatorPattern = regexp.MustCompile(`^(in|out)/[0-9a-f-]{36}(\.[0-9]+\.[a-z0-9]+)?$`)
	inKeyRe        = regexp.MustCompile(`^in/[0-9a-f-]{36}$`)
	outKeyRe       = regexp.MustCompile(`^out/[0-9a-f-]{36}\.[0-9]+\.[a-z0-9]+$`)
	keyIDRe        = regexp.MustCompile(`^[0-9a-f-]{36}$`)
	extRe          = regexp.MustCompile(`^[a-z0-9]+$`)
)

func invalidKey(op string) error { return fmt.Errorf("blob: s3 %s: %w", op, ErrInvalidLocator) }

// NewS3 builds an S3 store. It does not contact the endpoint (see Check).
func NewS3(o S3Options) (*S3, error) {
	if o.Bucket == "" || o.Endpoint == "" {
		return nil, errors.New("blob: s3 endpoint and bucket are required")
	}
	if o.Region == "" {
		o.Region = "us-east-1" // set explicitly: presigning never needs a bucket-location request
	}
	c, err := newMinio(o.Endpoint, o.UseSSL, o)
	if err != nil {
		return nil, err
	}
	pubEP := o.PublicEndpoint
	if pubEP == "" {
		pubEP = "http://" + o.Endpoint
		if o.UseSSL {
			pubEP = "https://" + o.Endpoint
		}
	}
	u, err := url.Parse(pubEP)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("blob: invalid s3 public endpoint")
	}
	pub, err := newMinio(u.Host, u.Scheme == "https", o)
	if err != nil {
		return nil, err
	}
	scratch := o.ScratchDir
	if scratch == "" {
		scratch = os.TempDir()
	}
	s := &S3{c: c, pub: pub, bucket: o.Bucket, scratch: scratch, maxInput: o.MaxInputBytes}
	s.origin = s.signedOrigin()
	return s, nil
}

// signedOrigin signs a throwaway upload form (a local computation: the region is set, so no
// request is made) and returns the origin of its URL. Deriving the origin from a real signature
// rather than from the configuration keeps it right for both path-style (host) and
// virtual-host-style (bucket.host) addressing.
func (s *S3) signedOrigin() string {
	pp, err := s.PresignUpload(context.Background(), "00000000-0000-0000-0000-000000000000", 1, time.Minute)
	if err != nil {
		return ""
	}
	u, err := url.Parse(pp.URL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// UploadOrigin returns the origin presigned upload forms are posted to ("" if it is unknown).
func (s *S3) UploadOrigin() string { return s.origin }

func newMinio(endpoint string, secure bool, o S3Options) (*minio.Client, error) {
	lookup := minio.BucketLookupDNS
	if o.PathStyle {
		lookup = minio.BucketLookupPath
	}
	return minio.New(endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(o.AccessKey, o.SecretKey, ""),
		Secure:       secure,
		Region:       o.Region,
		BucketLookup: lookup,
		Transport:    o.Transport,
	})
}

// ErrNoBucket is returned by Check when the configured bucket does not exist.
var ErrNoBucket = errors.New("blob: s3 bucket does not exist")

// Check verifies that the bucket exists and the credentials can see it.
func (s *S3) Check(ctx context.Context) error {
	ok, err := s.c.BucketExists(ctx, s.bucket)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNoBucket
	}
	return nil
}

// mapErr converts "object does not exist" into fs.ErrNotExist (the Store contract) and leaves
// every other error (network, permissions, missing bucket) as is: those are not proof of absence.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	switch minio.ToErrorResponse(err).Code {
	case minio.NoSuchKey:
		return fmt.Errorf("blob: s3 object: %w", fs.ErrNotExist)
	}
	return err
}

// absent reports whether key does not exist yet (the no-overwrite check).
func (s *S3) absent(ctx context.Context, key string) error {
	_, err := s.c.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	switch err = mapErr(err); {
	case err == nil:
		return fmt.Errorf("blob: s3 object already exists: %w", fs.ErrExist)
	case errors.Is(err, fs.ErrNotExist):
		return nil
	default:
		return err
	}
}

// streamUpload feeds a PutObject running in its own goroutine through a pipe.
type streamUpload struct {
	pw   *io.PipeWriter
	done chan error
	err  error
	shut bool
}

func (u *streamUpload) Write(p []byte) (int, error) { return u.pw.Write(p) }

// Close finishes the upload and returns its result.
func (u *streamUpload) Close() error { return u.finish(nil) }

// CloseWithError aborts the upload (the multipart upload is aborted, nothing is stored).
func (u *streamUpload) CloseWithError(err error) error {
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return u.finish(err)
}

func (u *streamUpload) finish(cause error) error {
	if u.shut {
		return u.err
	}
	u.shut = true
	if cause != nil {
		_ = u.pw.CloseWithError(cause)
	} else {
		_ = u.pw.Close()
	}
	u.err = <-u.done
	return u.err
}

// CreateInput streams the input to in/<id>. Writes go straight to a multipart upload (one part
// of s3PartSize is buffered at a time). The key must not exist yet: an existence check runs
// first (S3 conditional writes, If-None-Match, are not applied by minio-go to the completion of a
// multipart upload, so they cannot be used for a stream of unknown length). ids are fresh UUIDs,
// so the check is defense in depth. Call CloseWithError to abort.
func (s *S3) CreateInput(ctx context.Context, id string) (io.WriteCloser, string, error) {
	if !keyIDRe.MatchString(id) {
		return nil, "", errors.New("blob: invalid input id")
	}
	key := KindIn.String() + "/" + id
	if err := s.absent(ctx, key); err != nil {
		return nil, "", err
	}
	pr, pw := io.Pipe()
	u := &streamUpload{pw: pw, done: make(chan error, 1)}
	go func() {
		_, err := s.c.PutObject(ctx, s.bucket, key, pr, -1, minio.PutObjectOptions{
			PartSize: s3PartSize, ContentType: "application/octet-stream"})
		if err == nil {
			_ = pr.CloseWithError(io.ErrClosedPipe) // late writes fail instead of blocking
		} else {
			_ = pr.CloseWithError(err)
		}
		u.done <- err
	}()
	return u, key, nil
}

// OpenOutput returns the object as a ReadSeekCloser: minio-go's Object issues ranged GETs on
// demand (a Seek followed by a Read starts a new ranged request), so http.ServeContent can answer
// Range requests without downloading the whole output or using local disk.
func (s *S3) OpenOutput(ctx context.Context, locator string) (io.ReadSeekCloser, Info, error) {
	if locator == "" {
		return nil, Info{}, fs.ErrNotExist
	}
	if !outKeyRe.MatchString(locator) {
		return nil, Info{}, invalidKey("open output")
	}
	obj, err := s.c.GetObject(ctx, s.bucket, locator, minio.GetObjectOptions{})
	if err != nil {
		return nil, Info{}, mapErr(err)
	}
	st, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		return nil, Info{}, mapErr(err)
	}
	return obj, Info{Size: st.Size, ModTime: st.LastModified}, nil
}

// LocalInput downloads the input into a private temporary file in scratchDir; cleanup removes it.
func (s *S3) LocalInput(ctx context.Context, locator, scratchDir string) (string, func(), error) {
	if locator == "" {
		return "", nil, fs.ErrNotExist
	}
	if !inKeyRe.MatchString(locator) {
		return "", nil, invalidKey("local input")
	}
	if scratchDir == "" {
		scratchDir = s.scratch
	}
	obj, err := s.c.GetObject(ctx, s.bucket, locator, minio.GetObjectOptions{})
	if err != nil {
		return "", nil, mapErr(err)
	}
	defer obj.Close()
	st, err := obj.Stat()
	if err != nil {
		return "", nil, mapErr(err)
	}
	if s.maxInput > 0 && st.Size > s.maxInput {
		return "", nil, ErrTooLarge
	}
	f, err := os.CreateTemp(scratchDir, "input-*") // mode 0600
	if err != nil {
		return "", nil, err
	}
	p := f.Name()
	cleanup := func() { _ = os.Remove(p) }
	var src io.Reader = obj
	if s.maxInput > 0 {
		src = io.LimitReader(obj, s.maxInput+1) // the object may change between Stat and the GET
	}
	n, err := io.Copy(f, src)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && s.maxInput > 0 && n > s.maxInput {
		err = ErrTooLarge
	}
	if err != nil {
		cleanup()
		return "", nil, mapErr(err)
	}
	return p, cleanup, nil
}

// NewOutput returns a local file name in the scratch directory; commit uploads it to
// out/<id>.<attempt>.<ext> (refusing to overwrite an existing object) and removes the local file.
func (s *S3) NewOutput(_ context.Context, jobID string, attempt int, ext string) (string, func(context.Context) (string, error), error) {
	if !keyIDRe.MatchString(jobID) || !extRe.MatchString(ext) || attempt < 0 {
		return "", nil, errors.New("blob: invalid output name")
	}
	name := jobID + "." + strconv.Itoa(attempt) + "." + ext
	key := KindOut.String() + "/" + name
	local := filepath.Join(s.scratch, "output-"+name)
	commit := func(ctx context.Context) (string, error) {
		st, err := os.Stat(local)
		if err != nil {
			return "", err
		}
		if err := s.absent(ctx, key); err != nil {
			return "", err
		}
		opts := minio.PutObjectOptions{PartSize: s3PartSize, ContentType: "application/octet-stream"}
		if st.Size() <= s3PartSize {
			opts.SetMatchETagExcept("*") // single PUT: If-None-Match: * makes the write conditional
		}
		_, err = s.c.FPutObject(ctx, s.bucket, key, local, opts)
		if err != nil && minio.ToErrorResponse(err).Code == "NotImplemented" {
			// The store does not support conditional writes: rely on the existence check above.
			_, err = s.c.FPutObject(ctx, s.bucket, key, local, minio.PutObjectOptions{PartSize: s3PartSize, ContentType: "application/octet-stream"})
		}
		if err != nil {
			return "", err
		}
		_ = os.Remove(local)
		return key, nil
	}
	return local, commit, nil
}

// Remove deletes an object. Only keys of this store's form are accepted (ErrInvalidLocator
// otherwise). A missing object is success.
func (s *S3) Remove(ctx context.Context, locator string) error {
	if locator == "" {
		return nil
	}
	if !inKeyRe.MatchString(locator) && !outKeyRe.MatchString(locator) {
		return invalidKey("remove")
	}
	err := mapErr(s.c.RemoveObject(ctx, s.bucket, locator, minio.RemoveObjectOptions{}))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// List returns the objects directly under in/ or out/ (keys with a further "/" are skipped).
// An empty namespace is an empty list.
func (s *S3) List(ctx context.Context, kind Kind) ([]Entry, error) {
	prefix := kind.String() + "/"
	var out []Entry
	for o := range s.c.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if o.Err != nil {
			return out, o.Err
		}
		name := o.Key[len(prefix):]
		if name == "" || filepath.Base(name) != name {
			continue
		}
		out = append(out, Entry{Name: name, Locator: o.Key, Info: Info{Size: o.Size, ModTime: o.LastModified}})
	}
	return out, ctx.Err()
}

// FreeBytes is not meaningful for an object store.
func (s *S3) FreeBytes(context.Context) (int64, error) {
	return 0, fmt.Errorf("blob: free space of an s3 bucket: %w", errors.ErrUnsupported)
}

// PresignUpload signs a POST policy that pins the bucket, the exact key in/<id>, a content length
// of 1..maxBytes and the expiry; no other form field (ACL, content type, metadata, redirect) is
// allowed by the policy, so the store rejects any form that adds one.
func (s *S3) PresignUpload(ctx context.Context, id string, maxBytes int64, ttl time.Duration) (PresignedPost, error) {
	if !keyIDRe.MatchString(id) {
		return PresignedPost{}, errors.New("blob: invalid input id")
	}
	if maxBytes < 1 || ttl <= 0 {
		return PresignedPost{}, errors.New("blob: invalid presign limits")
	}
	key := KindIn.String() + "/" + id
	p := minio.NewPostPolicy()
	if err := p.SetBucket(s.bucket); err != nil {
		return PresignedPost{}, err
	}
	if err := p.SetKey(key); err != nil {
		return PresignedPost{}, err
	}
	if err := p.SetExpires(time.Now().UTC().Add(ttl)); err != nil {
		return PresignedPost{}, err
	}
	if err := p.SetContentLengthRange(1, maxBytes); err != nil {
		return PresignedPost{}, err
	}
	u, fields, err := s.pub.PresignedPostPolicy(ctx, p)
	if err != nil {
		return PresignedPost{}, err
	}
	return PresignedPost{URL: u.String(), Fields: fields, Key: key}, nil
}

// StatInput returns the size of an uploaded input.
func (s *S3) StatInput(ctx context.Context, locator string) (int64, error) {
	if !inKeyRe.MatchString(locator) {
		return 0, invalidKey("stat input")
	}
	st, err := s.c.StatObject(ctx, s.bucket, locator, minio.StatObjectOptions{})
	if err != nil {
		return 0, mapErr(err)
	}
	return st.Size, nil
}

// PresignDownload checks that the output exists (fs.ErrNotExist otherwise) and signs a GET for
// the public endpoint. The response headers are part of the signed query, so they cannot be
// changed by whoever holds the URL; Cache-Control is always no-store.
func (s *S3) PresignDownload(ctx context.Context, locator string, h DownloadHeaders, ttl time.Duration) (string, error) {
	if !outKeyRe.MatchString(locator) {
		return "", invalidKey("presign download")
	}
	if _, err := s.c.StatObject(ctx, s.bucket, locator, minio.StatObjectOptions{}); err != nil {
		return "", mapErr(err)
	}
	q := url.Values{}
	q.Set("response-cache-control", "no-store")
	if h.ContentDisposition != "" {
		q.Set("response-content-disposition", h.ContentDisposition)
	}
	if h.ContentType != "" {
		q.Set("response-content-type", h.ContentType)
	}
	u, err := s.pub.PresignedGetObject(ctx, s.bucket, locator, ttl, q)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}
