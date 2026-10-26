package blob_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ShifuRain/convertflow/internal/blob"
)

// unreachable: no test here may contact the endpoint (validation and signing are local).
func offlineS3(t *testing.T) *blob.S3 {
	t.Helper()
	s, err := blob.NewS3(blob.S3Options{Endpoint: "127.0.0.1:1", PublicEndpoint: "https://files.example.org:9443",
		Bucket: "cf-bucket", Region: "eu-central-1", PathStyle: true, AccessKey: "AKIDEXAMPLE", SecretKey: "s3cr3t-value",
		ScratchDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const tid = "0b5c3a8e-6f1d-4c1e-9a55-2f7d2c1b9e10"

func TestLocatorPattern(t *testing.T) {
	good := []string{"in/" + tid, "out/" + tid + ".1.webm", "out/" + tid + ".12.mp3"}
	bad := []string{"", "in/", "in/" + tid + "/x", "/in/" + tid, "in/../" + tid, "IN/" + tid, "in/" + strings.ToUpper(tid),
		"tmp/" + tid, "out/" + tid + ".1.WEBM", "out/" + tid + ".x.webm", "out/" + tid + ".1.", "in/" + tid + "\n",
		"in/" + tid[:35], "in/" + tid + "0", "out/" + tid + ".1.web m", "in/" + tid + "?x", "../in/" + tid}
	for _, k := range good {
		if !blob.LocatorPattern.MatchString(k) {
			t.Errorf("%q must be accepted", k)
		}
	}
	for _, k := range bad {
		if blob.LocatorPattern.MatchString(k) {
			t.Errorf("%q must be refused", k)
		}
	}
}

// Every read and the remove refuse foreign keys before any request is made; reads also require
// the right namespace (an output key is never downloaded as an input, and vice versa).
func TestS3RefusesForeignLocators(t *testing.T) {
	s := offlineS3(t)
	in, out := "in/"+tid, "out/"+tid+".1.webm"
	for _, loc := range []string{out, "../etc/passwd", "/etc/passwd", "in/" + tid + "/x", "other/" + tid} {
		if _, _, err := s.LocalInput(ctx, loc, t.TempDir()); !errors.Is(err, blob.ErrInvalidLocator) {
			t.Errorf("LocalInput(%q) = %v", loc, err)
		}
		if _, err := s.StatInput(ctx, loc); !errors.Is(err, blob.ErrInvalidLocator) {
			t.Errorf("StatInput(%q) = %v", loc, err)
		}
	}
	for _, loc := range []string{in, "../etc/passwd", "out/" + tid, "out/x.1.webm"} {
		if _, _, err := s.OpenOutput(ctx, loc); !errors.Is(err, blob.ErrInvalidLocator) {
			t.Errorf("OpenOutput(%q) = %v", loc, err)
		}
		if _, err := s.PresignDownload(ctx, loc, blob.DownloadHeaders{}, time.Minute); !errors.Is(err, blob.ErrInvalidLocator) {
			t.Errorf("PresignDownload(%q) = %v", loc, err)
		}
	}
	for _, loc := range []string{"/etc/passwd", "in/x", "in/" + tid + "/../y", "out/" + tid} {
		if err := s.Remove(ctx, loc); !errors.Is(err, blob.ErrInvalidLocator) {
			t.Errorf("Remove(%q) = %v", loc, err)
		}
	}
	if err := s.Remove(ctx, ""); err != nil {
		t.Errorf("Remove(\"\") = %v", err)
	}
	if _, _, err := s.OpenOutput(ctx, ""); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("OpenOutput(\"\") = %v", err)
	}
	for _, id := range []string{"", "../x", tid + "/x", strings.ToUpper(tid)} {
		if _, _, err := s.CreateInput(ctx, id); err == nil {
			t.Errorf("CreateInput(%q) accepted", id)
		}
		if _, err := s.PresignUpload(ctx, id, 10, time.Minute); err == nil {
			t.Errorf("PresignUpload(%q) accepted", id)
		}
	}
	if _, _, err := s.NewOutput(ctx, tid, 1, "../x"); err == nil {
		t.Error("NewOutput accepted a bad extension")
	}
	if _, _, err := s.NewOutput(ctx, "x.y", 1, "webm"); err == nil {
		t.Error("NewOutput accepted a bad job id")
	}
}

func TestS3FreeBytesUnsupported(t *testing.T) {
	if _, err := offlineS3(t).FreeBytes(ctx); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("FreeBytes = %v, want ErrUnsupported", err)
	}
}

// The signed policy pins every condition and nothing else: bucket, exact key, content length,
// expiry and the signature parameters. The form carries no field the policy does not cover.
func TestS3PresignUploadPolicy(t *testing.T) {
	s := offlineS3(t)
	before := time.Now().UTC()
	pp, err := s.PresignUpload(ctx, tid, 12345, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if pp.Key != "in/"+tid {
		t.Fatalf("key %q", pp.Key)
	}
	u, err := url.Parse(pp.URL)
	if err != nil || u.Scheme != "https" || u.Host != "files.example.org:9443" || strings.TrimSuffix(u.Path, "/") != "/cf-bucket" || u.RawQuery != "" {
		t.Fatalf("upload URL %q must target the public endpoint, path-style bucket", pp.URL)
	}
	var fields []string
	for k := range pp.Fields {
		fields = append(fields, k)
	}
	sort.Strings(fields)
	want := []string{"bucket", "key", "policy", "x-amz-algorithm", "x-amz-credential", "x-amz-date", "x-amz-signature"}
	if !reflect.DeepEqual(fields, want) {
		t.Fatalf("form fields %v, want %v", fields, want)
	}
	if pp.Fields["key"] != "in/"+tid || pp.Fields["bucket"] != "cf-bucket" || pp.Fields["x-amz-algorithm"] != "AWS4-HMAC-SHA256" {
		t.Fatalf("fields %v", pp.Fields)
	}
	if !strings.HasPrefix(pp.Fields["x-amz-credential"], "AKIDEXAMPLE/") || !strings.Contains(pp.Fields["x-amz-credential"], "/eu-central-1/s3/aws4_request") {
		t.Fatalf("credential scope %q", pp.Fields["x-amz-credential"])
	}
	for _, v := range pp.Fields {
		if strings.Contains(v, "s3cr3t-value") {
			t.Fatal("the secret key must never appear in the form")
		}
	}

	raw, err := base64.StdEncoding.DecodeString(pp.Fields["policy"])
	if err != nil {
		t.Fatal(err)
	}
	var pol struct {
		Expiration string `json:"expiration"`
		Conditions []any  `json:"conditions"`
	}
	if err := json.Unmarshal(raw, &pol); err != nil {
		t.Fatalf("policy %s: %v", raw, err)
	}
	exp, err := time.Parse("2006-01-02T15:04:05.000Z", pol.Expiration)
	if err != nil {
		t.Fatal(err)
	}
	if d := exp.Sub(before); d < 9*time.Minute || d > 11*time.Minute {
		t.Fatalf("expiration %s is not ~10m ahead", pol.Expiration)
	}
	got := map[string]string{}
	var lengthRange []any
	for _, c := range pol.Conditions {
		arr, ok := c.([]any)
		if !ok {
			t.Fatalf("condition %v is not an array (object conditions are not used)", c)
		}
		if arr[0] == "content-length-range" {
			lengthRange = arr
			continue
		}
		if arr[0] != "eq" {
			t.Fatalf("only exact-match conditions are allowed, got %v", arr)
		}
		got[arr[1].(string)] = arr[2].(string)
	}
	if len(got) != 5 || got["$bucket"] != "cf-bucket" || got["$key"] != "in/"+tid ||
		got["$x-amz-algorithm"] != "AWS4-HMAC-SHA256" || got["$x-amz-credential"] != pp.Fields["x-amz-credential"] ||
		got["$x-amz-date"] != pp.Fields["x-amz-date"] {
		t.Fatalf("conditions %v", got)
	}
	if len(lengthRange) != 3 || lengthRange[1] != float64(1) || lengthRange[2] != float64(12345) {
		t.Fatalf("content-length-range %v", lengthRange)
	}
	if strings.Contains(string(raw), "starts-with") || strings.Contains(strings.ToLower(string(raw)), "acl") {
		t.Fatalf("policy must not contain prefix matches or ACLs: %s", raw)
	}

	if _, err := s.PresignUpload(ctx, tid, 0, time.Minute); err == nil {
		t.Error("maxBytes 0 must be refused")
	}
	if _, err := s.PresignUpload(ctx, tid, 10, 0); err == nil {
		t.Error("ttl 0 must be refused")
	}
}
