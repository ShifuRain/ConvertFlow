package blob_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ShifuRain/convertflow/internal/blob"
	"github.com/ShifuRain/convertflow/internal/blob/s3test"
	"github.com/google/uuid"
)

// postForm sends a browser-style upload: every field, then the file as the last part.
func postForm(t *testing.T, target string, fields map[string]string, extra [][2]string, body []byte) (int, string) {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	for _, kv := range extra {
		_ = mw.WriteField(kv[0], kv[1])
	}
	fw, _ := mw.CreateFormFile("file", "clip.mkv")
	_, _ = fw.Write(body)
	_ = mw.Close()
	resp, err := http.Post(target, mw.FormDataContentType(), &b)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(msg)
}

func s3Code(body string) string {
	if i := strings.Index(body, "<Code>"); i >= 0 {
		if j := strings.Index(body[i:], "</Code>"); j > 0 {
			return body[i+6 : i+j]
		}
	}
	return ""
}

func TestS3RoundTrip(t *testing.T) {
	e := s3test.New(t)
	s := e.Store(t)
	id := uuid.NewString()
	payload := bytes.Repeat([]byte("0123456789"), 1000)

	w, loc, err := s.CreateInput(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if loc != "in/"+id {
		t.Fatalf("locator %q", loc)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := s.StatInput(ctx, loc); err != nil || n != int64(len(payload)) {
		t.Fatalf("StatInput = %d %v", n, err)
	}
	// No overwrite: the same id cannot be created twice.
	if _, _, err := s.CreateInput(ctx, id); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second CreateInput = %v, want ErrExist", err)
	}

	scratch := t.TempDir()
	p, done, err := s.LocalInput(ctx, loc, scratch)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(p) != scratch {
		t.Fatalf("local copy %q not in scratch", p)
	}
	if b, _ := os.ReadFile(p); !bytes.Equal(b, payload) {
		t.Fatal("local copy differs")
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("local copy mode %v", st.Mode())
	}
	done()
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("cleanup must remove the local copy")
	}

	outPath, commit, err := s.NewOutput(ctx, id, 2, "webm")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outPath, []byte("output-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	outLoc, err := commit(ctx)
	if err != nil || outLoc != "out/"+id+".2.webm" {
		t.Fatalf("commit = %q %v", outLoc, err)
	}
	if _, err := os.Stat(outPath); !os.IsNotExist(err) {
		t.Fatal("commit must remove the local output")
	}
	// A second commit to the same key is refused (no overwrite).
	_ = os.WriteFile(outPath, []byte("other"), 0o600)
	if _, err := commit(ctx); err == nil {
		t.Fatal("committing over an existing output must fail")
	}
	_ = os.Remove(outPath)

	r, info, err := s.OpenOutput(ctx, outLoc)
	if err != nil || info.Size != 12 || info.ModTime.IsZero() {
		t.Fatalf("OpenOutput: %+v %v", info, err)
	}
	if _, err := r.Seek(7, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(r); string(b) != "bytes" {
		t.Fatalf("ranged read %q", b)
	}
	_ = r.Close()

	ins, err := s.List(ctx, blob.KindIn)
	if err != nil || len(ins) != 1 || ins[0].Name != id || ins[0].Locator != loc || ins[0].Info.Size != int64(len(payload)) || ins[0].Info.ModTime.IsZero() {
		t.Fatalf("List(in) = %+v %v", ins, err)
	}
	outs, err := s.List(ctx, blob.KindOut)
	if err != nil || len(outs) != 1 || outs[0].Name != id+".2.webm" || outs[0].Locator != outLoc {
		t.Fatalf("List(out) = %+v %v", outs, err)
	}

	for _, l := range []string{loc, outLoc, loc} { // removing twice is fine
		if err := s.Remove(ctx, l); err != nil {
			t.Fatalf("Remove(%q) = %v", l, err)
		}
	}
	if ins, _ := s.List(ctx, blob.KindIn); len(ins) != 0 {
		t.Fatalf("List(in) after remove = %+v", ins)
	}
	if _, _, err := s.OpenOutput(ctx, outLoc); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("OpenOutput(missing) = %v", err)
	}
	if _, _, err := s.LocalInput(ctx, loc, scratch); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("LocalInput(missing) = %v", err)
	}
	if _, err := s.StatInput(ctx, loc); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("StatInput(missing) = %v", err)
	}
	if _, err := s.PresignDownload(ctx, outLoc, blob.DownloadHeaders{}, time.Minute); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("PresignDownload(missing) = %v", err)
	}
	if ents, _ := os.ReadDir(scratch); len(ents) != 0 {
		t.Fatalf("scratch not empty: %v", ents)
	}
}

// An aborted stream stores nothing.
func TestS3CreateInputAbort(t *testing.T) {
	e := s3test.New(t)
	s := e.Store(t)
	w, loc, err := s.CreateInput(ctx, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("partial"))
	ab, ok := w.(interface{ CloseWithError(error) error })
	if !ok {
		t.Fatal("the stream must support CloseWithError")
	}
	if err := ab.CloseWithError(errors.New("client went away")); err == nil {
		t.Fatal("an aborted upload must report an error")
	}
	if _, err := s.StatInput(ctx, loc); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("aborted upload left an object: %v", err)
	}
}

// LocalInput refuses an input larger than MaxInputBytes (an object replaced after the API's check).
func TestS3LocalInputSizeCap(t *testing.T) {
	e := s3test.New(t)
	e.Opts.MaxInputBytes = 4
	s := e.Store(t)
	w, loc, _ := s.CreateInput(ctx, uuid.NewString())
	_, _ = w.Write([]byte("12345"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	if _, _, err := s.LocalInput(ctx, loc, scratch); !errors.Is(err, blob.ErrTooLarge) {
		t.Fatalf("LocalInput = %v, want ErrTooLarge", err)
	}
	if ents, _ := os.ReadDir(scratch); len(ents) != 0 {
		t.Fatal("refused input left a file")
	}
}

func TestS3PresignedPostAccepted(t *testing.T) {
	e := s3test.New(t)
	s := e.Store(t)
	id := uuid.NewString()
	pp, err := s.PresignUpload(ctx, id, 1000, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := postForm(t, pp.URL, pp.Fields, nil, []byte("hello world")); code/100 != 2 {
		t.Fatalf("upload refused: %d %s", code, body)
	}
	if n, err := s.StatInput(ctx, pp.Key); err != nil || n != 11 {
		t.Fatalf("StatInput = %d %v", n, err)
	}
}

// The bucket (not the API) enforces content-length-range; the API's StatInput check is the
// authority regardless. Empty uploads are refused as well (minimum 1 byte).
func TestS3PresignedPostSizeLimits(t *testing.T) {
	e := s3test.New(t)
	s := e.Store(t)
	for name, size := range map[string]int{"oversize": 11, "empty": 0} {
		id := uuid.NewString()
		pp, err := s.PresignUpload(ctx, id, 10, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		code, body := postForm(t, pp.URL, pp.Fields, nil, bytes.Repeat([]byte("x"), size))
		t.Logf("%s upload: HTTP %d, S3 code %q", name, code, s3Code(body))
		if code/100 == 2 {
			t.Fatalf("%s upload accepted by the bucket", name)
		}
		if _, err := s.StatInput(ctx, pp.Key); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s upload stored an object: %v", name, err)
		}
	}
	// The exact limit is accepted.
	pp, _ := s.PresignUpload(ctx, uuid.NewString(), 10, time.Minute)
	if code, body := postForm(t, pp.URL, pp.Fields, nil, bytes.Repeat([]byte("x"), 10)); code/100 != 2 {
		t.Fatalf("upload of exactly the limit refused: %d %s", code, body)
	}
}

// A form for one job cannot create or overwrite any other key, and cannot add fields the policy
// does not cover (ACL, content type, metadata).
func TestS3PresignedPostForgery(t *testing.T) {
	e := s3test.New(t)
	s := e.Store(t)
	id, other := uuid.NewString(), uuid.NewString()
	pp, err := s.PresignUpload(ctx, id, 1000, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// Seed the victim object so an overwrite would be visible.
	w, otherLoc, _ := s.CreateInput(ctx, other)
	_, _ = w.Write([]byte("victim"))
	_ = w.Close()

	forged := func(mut func(map[string]string)) map[string]string {
		m := map[string]string{}
		for k, v := range pp.Fields {
			m[k] = v
		}
		mut(m)
		return m
	}
	cases := map[string]struct {
		fields map[string]string
		extra  [][2]string
	}{
		"other key":      {fields: forged(func(m map[string]string) { m["key"] = otherLoc })},
		"output key":     {fields: forged(func(m map[string]string) { m["key"] = "out/" + id + ".1.webm" })},
		"key template":   {fields: forged(func(m map[string]string) { m["key"] = "in/${filename}" })},
		"acl":            {fields: pp.Fields, extra: [][2]string{{"acl", "public-read"}}},
		"content type":   {fields: pp.Fields, extra: [][2]string{{"Content-Type", "text/html"}}},
		"metadata":       {fields: pp.Fields, extra: [][2]string{{"x-amz-meta-x", "y"}}},
		"redirect":       {fields: pp.Fields, extra: [][2]string{{"success_action_redirect", "https://evil.example/"}}},
		"tampered limit": {fields: forged(func(m map[string]string) { m["policy"] = tamperPolicy(t, m["policy"]) })},
	}
	for name, c := range cases {
		code, body := postForm(t, pp.URL, c.fields, c.extra, []byte("attacker"))
		t.Logf("%s: HTTP %d, S3 code %q", name, code, s3Code(body))
		if code/100 == 2 {
			t.Errorf("%s: forged upload accepted", name)
		}
	}
	if r, _, err := s.LocalInput(ctx, otherLoc, t.TempDir()); err == nil {
		if b, _ := os.ReadFile(r); string(b) != "victim" {
			t.Fatal("the other job's input was overwritten")
		}
	} else {
		t.Fatal(err)
	}
	if _, err := s.StatInput(ctx, pp.Key); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a forged form created the job's own input: %v", err)
	}
	if outs, _ := s.List(ctx, blob.KindOut); len(outs) != 0 {
		t.Fatalf("a forged form created an output: %v", outs)
	}
}

// tamperPolicy raises the content-length maximum inside the (signed) policy document.
func tamperPolicy(t *testing.T, b64 string) string {
	t.Helper()
	raw, err := decodeB64(b64)
	if err != nil {
		t.Fatal(err)
	}
	out := strings.Replace(string(raw), "1000]", "100000000]", 1)
	if out == string(raw) {
		t.Fatalf("policy has no length limit to tamper with: %s", raw)
	}
	return encodeB64([]byte(out))
}

func TestS3PresignedPostExpired(t *testing.T) {
	e := s3test.New(t)
	s := e.Store(t)
	pp, err := s.PresignUpload(ctx, uuid.NewString(), 1000, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2500 * time.Millisecond)
	code, body := postForm(t, pp.URL, pp.Fields, nil, []byte("late"))
	t.Logf("expired: HTTP %d, S3 code %q", code, s3Code(body))
	if code/100 == 2 {
		t.Fatal("expired policy accepted")
	}
	if _, err := s.StatInput(ctx, pp.Key); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expired upload stored an object: %v", err)
	}
}

func TestS3PresignedDownload(t *testing.T) {
	e := s3test.New(t)
	s := e.Store(t)
	id := uuid.NewString()
	outPath, commit, _ := s.NewOutput(ctx, id, 1, "mp3")
	_ = os.WriteFile(outPath, []byte("ID3-audio"), 0o600)
	loc, err := commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cd := `attachment; filename="song.mp3"; filename*=UTF-8''song.mp3`
	u, err := s.PresignDownload(ctx, loc, blob.DownloadHeaders{ContentDisposition: cd, ContentType: "audio/mpeg"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u, e.Opts.PublicEndpoint+"/") {
		t.Fatalf("URL %q is not on the public endpoint", u)
	}
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "ID3-audio" {
		t.Fatalf("GET: %d %q", resp.StatusCode, b)
	}
	h := resp.Header
	if h.Get("Content-Disposition") != cd || h.Get("Content-Type") != "audio/mpeg" || h.Get("Cache-Control") != "no-store" {
		t.Fatalf("headers: %v", h)
	}
	// The response headers are signed: changing them invalidates the URL.
	pu, _ := url.Parse(u)
	q := pu.Query()
	q.Set("response-content-type", "text/html")
	pu.RawQuery = q.Encode()
	resp, err = http.Get(pu.String())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("tampered URL: %d, want 403", resp.StatusCode)
	}
	// Expired download URL.
	u, _ = s.PresignDownload(ctx, loc, blob.DownloadHeaders{}, time.Second)
	time.Sleep(2500 * time.Millisecond)
	resp, err = http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expired URL: %d, want 403", resp.StatusCode)
	}
}

func TestS3CheckBucket(t *testing.T) {
	e := s3test.New(t)
	if err := e.Store(t).Check(ctx); err != nil {
		t.Fatal(err)
	}
	e.Opts.Bucket = "cf-test-does-not-exist-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	if err := e.Store(t).Check(ctx); !errors.Is(err, blob.ErrNoBucket) {
		t.Fatalf("Check(missing bucket) = %v", err)
	}
}

func decodeB64(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }
func encodeB64(b []byte) string          { return base64.StdEncoding.EncodeToString(b) }
