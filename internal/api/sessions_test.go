package api_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ShifuRain/convertflow/internal/api"
	"github.com/ShifuRain/convertflow/internal/audit"
	"github.com/ShifuRain/convertflow/internal/config"
	"github.com/ShifuRain/convertflow/internal/db/dbtest"
	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/ShifuRain/convertflow/internal/presets"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// client is one browser (or curl with a cookie jar): it keeps the cookies the server sets.
type client struct {
	h       http.Handler
	remote  string
	cookies map[string]*http.Cookie
	hdr     map[string]string
	extra   []*http.Cookie // sent as is, in addition (planted cookies)
}

func newClient(h http.Handler, remote string) *client {
	return &client{h: h, remote: remote, cookies: map[string]*http.Cookie{}, hdr: map[string]string{}}
}

func (c *client) do(method, path string, body io.Reader, ctype string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, body)
	if ctype != "" {
		r.Header.Set("Content-Type", ctype)
	}
	r.RemoteAddr = c.remote
	for k, v := range c.hdr {
		r.Header.Set(k, v)
	}
	for _, ck := range c.cookies {
		r.AddCookie(&http.Cookie{Name: ck.Name, Value: ck.Value})
	}
	for _, ck := range c.extra {
		r.AddCookie(ck)
	}
	w := httptest.NewRecorder()
	c.h.ServeHTTP(w, r)
	for _, ck := range w.Result().Cookies() {
		if ck.MaxAge < 0 {
			delete(c.cookies, ck.Name)
		} else {
			c.cookies[ck.Name] = ck
		}
	}
	return w
}

func (c *client) get(path string) *httptest.ResponseRecorder { return c.do("GET", path, nil, "") }

func (c *client) upload(t *testing.T, size int) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, "a.wav", bytes.Repeat([]byte{7}, size))
	return c.do("POST", "/api/v1/jobs", body, ct)
}

func (c *client) createID(t *testing.T) string {
	t.Helper()
	w := c.upload(t, 4)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var v struct{ ID string }
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	return v.ID
}

func (c *client) listIDs(t *testing.T) []string {
	t.Helper()
	w := c.get("/api/v1/jobs")
	if w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	var v []struct{ ID string }
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, j := range v {
		out = append(out, j.ID)
	}
	return out
}

type sessFixture struct {
	auditFixture
	pool *pgxpool.Pool
}

// sessionFixture is newFixtureWith in cookie session mode that also exposes the database.
func sessionFixture(t *testing.T, mod func(*config.Config)) sessFixture {
	t.Helper()
	pool := dbtest.New(t)
	dir := t.TempDir()
	for _, d := range []string{"in", "out"} {
		_ = os.MkdirAll(filepath.Join(dir, d), 0o700)
	}
	store := jobs.NewStore(pool)
	_ = store.UpsertWorker(context.Background(), "w1", []string{"pcm_s16le", "libopus", "flac"})
	cfg := config.Config{AllowedHosts: []string{"*"}, DataDir: dir, MaxUploadBytes: 1 << 20, Retention: time.Hour,
		Sessions: config.SessionsCookie, SessionTTL: 24 * time.Hour, MaxConcurrentUploads: 64}
	if mod != nil {
		mod(&cfg)
	}
	var logBuf bytes.Buffer
	h := api.NewHandler(api.Deps{Store: store, Cfg: cfg, Audit: audit.New(&logBuf), EventInterval: 20 * time.Millisecond})
	return sessFixture{auditFixture{fixture{h, store, dir}, &logBuf}, pool}
}

func (f sessFixture) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ownerOf returns the owner key the store recorded for a job.
func ownerOf(t *testing.T, f fixture, id string) []byte {
	t.Helper()
	j, err := f.store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return j.Owner
}

// finish takes the oldest queued job through to done with an output file.
func finish(t *testing.T, f fixture, id string) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.store.Claim(ctx, "w1", presets.IDs(presets.All())); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(f.dir, "out", id+".1.wav")
	_ = os.WriteFile(out, []byte("RESULT"), 0o600)
	if err := f.store.SetRunning(ctx, id, "w1", "pcm_s16le"); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Complete(ctx, id, "w1", out); err != nil {
		t.Fatal(err)
	}
}

func TestSessionCookieIssuedWithFlags(t *testing.T) {
	f := sessionFixture(t, func(c *config.Config) { c.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")} })
	c := newClient(f.h, "192.0.2.10:1")
	w := c.get("/api/v1/presets")
	if w.Code != 200 {
		t.Fatalf("presets %d", w.Code)
	}
	raw := w.Header().Values("Set-Cookie")
	if len(raw) != 1 {
		t.Fatalf("Set-Cookie %v", raw)
	}
	for _, want := range []string{"cf_session=", "Path=/api/", "Max-Age=86400", "HttpOnly", "SameSite=Strict"} {
		if !strings.Contains(raw[0], want) {
			t.Errorf("cookie lacks %q: %s", want, raw[0])
		}
	}
	if strings.Contains(raw[0], "Secure") || strings.Contains(raw[0], "Domain") {
		t.Errorf("plain http cookie: %s", raw[0])
	}
	tok := c.cookies["cf_session"].Value
	if len(tok) != 43 {
		t.Fatalf("token %q", tok)
	}
	// the token is never logged, audited or stored
	if strings.Contains(f.log.String(), tok) {
		t.Fatal("token in the audit log")
	}
	if !strings.Contains(f.log.String(), `"event":"session_created"`) {
		t.Fatal("session creation not audited")
	}
	if f.count(t, `SELECT count(*) FROM sessions`) != 1 || f.count(t, `SELECT count(*) FROM sessions WHERE position(convert_to($1, 'UTF8') in key) > 0`, tok) != 0 {
		t.Fatal("session row missing, or the token itself stored")
	}
	// a second call with the cookie keeps the session (no new cookie)
	if w := c.get("/api/v1/presets"); len(w.Header().Values("Set-Cookie")) != 0 {
		t.Fatalf("session re-issued: %v", w.Header().Values("Set-Cookie"))
	}

	secure := func(tlsConn bool, remote, proto string) bool {
		r := httptest.NewRequest("GET", "/api/v1/presets", nil)
		if tlsConn {
			r.TLS = &tls.ConnectionState{}
		}
		r.RemoteAddr = remote
		if proto != "" {
			r.Header.Set("X-Forwarded-Proto", proto)
		}
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, r)
		v := rec.Header().Get("Set-Cookie")
		if v == "" {
			t.Fatal("no cookie")
		}
		return strings.Contains(v, "; Secure")
	}
	if !secure(true, "192.0.2.11:1", "") {
		t.Error("TLS connection: cookie not Secure")
	}
	if !secure(false, "10.0.0.5:1", "https") {
		t.Error("trusted TLS proxy: cookie not Secure")
	}
	if secure(false, "192.0.2.12:1", "https") {
		t.Error("untrusted X-Forwarded-Proto believed")
	}
}

// The isolation matrix: B and a client without a session can neither see nor touch A's jobs, and
// every such answer is the same 404 as for a job that does not exist.
func TestSessionIsolationMatrix(t *testing.T) {
	f := sessionFixture(t, nil)
	a, b, anon := newClient(f.h, "192.0.2.1:1"), newClient(f.h, "192.0.2.2:1"), newClient(f.h, "192.0.2.3:1")
	a.get("/api/v1/presets")
	b.get("/api/v1/presets")
	done := a.createID(t)
	finish(t, f.fixture, done)
	queued := a.createID(t)
	if ownerOf(t, f.fixture, done) == nil {
		t.Fatal("job not stamped with an owner")
	}
	bJob := b.createID(t)

	if ids := a.listIDs(t); len(ids) != 2 || !contains(ids, done) || !contains(ids, queued) {
		t.Fatalf("A lists %v", ids)
	}
	if ids := b.listIDs(t); len(ids) != 1 || ids[0] != bJob {
		t.Fatalf("B lists %v", ids)
	}
	if ids := anon.listIDs(t); len(ids) != 0 {
		t.Fatalf("no session lists %v", ids)
	}
	missing := uuid.NewString()
	ref := b.get("/api/v1/jobs/" + missing)
	for _, c := range []*client{b, anon} {
		for _, req := range []struct{ method, path string }{
			{"GET", "/api/v1/jobs/" + done}, {"GET", "/api/v1/jobs/" + done + "/output"},
			{"GET", "/api/v1/jobs/" + queued + "/events"}, {"DELETE", "/api/v1/jobs/" + queued},
			{"DELETE", "/api/v1/jobs/" + done}, {"POST", "/api/v1/jobs/" + queued + "/complete"},
		} {
			w := c.do(req.method, req.path, nil, "")
			if w.Code != http.StatusNotFound {
				t.Errorf("%s %s by a foreign client: %d %s", req.method, req.path, w.Code, w.Body)
			}
			if req.method == "GET" && !strings.HasSuffix(req.path, "/events") && !strings.HasSuffix(req.path, "/output") && w.Body.String() != ref.Body.String() {
				t.Errorf("foreign 404 differs from a missing job's: %s vs %s", w.Body, ref.Body)
			}
			if strings.Contains(w.Body.String(), "RESULT") {
				t.Fatal("output leaked")
			}
		}
	}
	// still there, untouched
	for _, id := range []string{done, queued} {
		j, err := f.store.Get(context.Background(), id)
		if err != nil || j.CancelRequested {
			t.Fatalf("job %s changed by a foreign request: %+v %v", id, j, err)
		}
	}
	// A can do everything
	if w := a.get("/api/v1/jobs/" + done + "/output"); w.Code != 200 || w.Body.String() != "RESULT" {
		t.Fatalf("A output: %d", w.Code)
	}
	if w := a.do("DELETE", "/api/v1/jobs/"+queued, nil, ""); w.Code != http.StatusNoContent {
		t.Fatalf("A delete: %d", w.Code)
	}
	// cancel of a running job: foreign 404, own 202
	if w := b.do("DELETE", "/api/v1/jobs/"+bJob, nil, ""); w.Code != http.StatusNoContent {
		t.Fatalf("B delete own: %d", w.Code)
	}
	running := a.createID(t)
	if _, err := f.store.Claim(context.Background(), "w1", presets.IDs(presets.All())); err != nil {
		t.Fatal(err)
	}
	if w := b.do("DELETE", "/api/v1/jobs/"+running, nil, ""); w.Code != http.StatusNotFound {
		t.Fatalf("B cancel: %d", w.Code)
	}
	if j, _ := f.store.Get(context.Background(), running); j.CancelRequested {
		t.Fatal("B's cancel reached A's job")
	}
	if w := a.do("DELETE", "/api/v1/jobs/"+running, nil, ""); w.Code != http.StatusAccepted {
		t.Fatalf("A cancel: %d %s", w.Code, w.Body)
	}
	// audit records carry a short owner prefix, never the token
	log := f.log.String()
	if !strings.Contains(log, `"owner":"`) || strings.Contains(log, a.cookies["cf_session"].Value) {
		t.Fatalf("audit owner: %.400s", log)
	}
}

func TestSessionSSEFiltered(t *testing.T) {
	f := sessionFixture(t, nil)
	a, b := newClient(f.h, "192.0.2.1:1"), newClient(f.h, "192.0.2.2:1")
	b.get("/api/v1/presets")
	id := a.createID(t)
	srv := httptest.NewServer(f.h)
	defer srv.Close()
	open := func(c *client) *http.Response {
		req, _ := http.NewRequest("GET", srv.URL+"/api/v1/jobs/"+id+"/events", nil)
		for _, ck := range c.cookies {
			req.AddCookie(&http.Cookie{Name: ck.Name, Value: ck.Value})
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		t.Cleanup(cancel)
		resp, err := http.DefaultClient.Do(req.WithContext(ctx))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	if resp := open(b); resp.StatusCode != 404 {
		t.Fatalf("B events: %d", resp.StatusCode)
	}
	resp := open(a)
	if resp.StatusCode != 200 {
		t.Fatalf("A events: %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			if !strings.Contains(sc.Text(), id) {
				t.Fatalf("event %s", sc.Text())
			}
			return
		}
	}
	t.Fatal("no event")
}

// Session fixation: a client-chosen token (well-formed or not) is never accepted; the server issues
// its own.
func TestSessionClientChosenTokenIgnored(t *testing.T) {
	f := sessionFixture(t, nil)
	victim := newClient(f.h, "192.0.2.1:1")
	victim.cookies["cf_session"] = &http.Cookie{Name: "cf_session", Value: strings.Repeat("A", 43)}
	id := victim.createID(t)
	if got := victim.cookies["cf_session"].Value; got == strings.Repeat("A", 43) || len(got) != 43 {
		t.Fatalf("chosen token kept: %q", got)
	}
	attacker := newClient(f.h, "192.0.2.9:1")
	attacker.cookies["cf_session"] = &http.Cookie{Name: "cf_session", Value: strings.Repeat("A", 43)}
	if ids := attacker.listIDs(t); len(ids) != 0 {
		t.Fatalf("attacker with the planted token sees %v", ids)
	}
	if w := attacker.get("/api/v1/jobs/" + id); w.Code != 404 {
		t.Fatalf("attacker: %d", w.Code)
	}
	// two session cookies (one planted for a narrower path): none is used and none is minted
	c := newClient(f.h, "192.0.2.1:1")
	c.cookies["cf_session"] = victim.cookies["cf_session"]
	c.extra = []*http.Cookie{{Name: "cf_session", Value: attacker.cookies["cf_session"].Value}}
	if ids := c.listIDs(t); len(ids) != 0 {
		t.Fatalf("conflicting cookies list %v", ids)
	}
	if w := c.upload(t, 4); w.Code != http.StatusBadRequest {
		t.Fatalf("create with conflicting cookies: %d", w.Code)
	}
}

func TestSessionExpiredIsNoSession(t *testing.T) {
	f := sessionFixture(t, func(c *config.Config) { c.SessionTTL = time.Hour })
	a := newClient(f.h, "192.0.2.1:1")
	id := a.createID(t)
	if n := f.count(t, `WITH u AS (UPDATE sessions SET last_seen = now() - interval '2 hours' RETURNING 1) SELECT count(*) FROM u`); n != 1 {
		t.Fatalf("aging the session: %d", n)
	}
	if ids := a.listIDs(t); len(ids) != 0 {
		t.Fatalf("expired session still lists %v", ids)
	}
	if w := a.get("/api/v1/jobs/" + id); w.Code != 404 {
		t.Fatalf("expired session: %d", w.Code)
	}
}

// SameSite=Strict keeps browsers from sending the cookie cross-site; the cross-origin guard
// refuses the request even if a cookie arrives (older browsers, same-site sibling hosts).
func TestCrossSiteRequestWithSessionCookieRefused(t *testing.T) {
	f := sessionFixture(t, nil)
	a := newClient(f.h, "192.0.2.1:1")
	id := a.createID(t)
	before := len(a.listIDs(t))
	a.hdr["Sec-Fetch-Site"] = "cross-site"
	a.hdr["Origin"] = "https://evil.example"
	if w := a.upload(t, 4); w.Code != http.StatusForbidden {
		t.Fatalf("cross-site POST with cookie: %d", w.Code)
	}
	if w := a.do("DELETE", "/api/v1/jobs/"+id, nil, ""); w.Code != http.StatusForbidden {
		t.Fatalf("cross-site DELETE with cookie: %d", w.Code)
	}
	if w := a.do("POST", "/api/v1/jobs/"+id+"/complete", nil, ""); w.Code != http.StatusForbidden {
		t.Fatalf("cross-site complete with cookie: %d", w.Code)
	}
	a.hdr["Sec-Fetch-Site"] = "same-site" // a sibling host is not same-origin either
	if w := a.do("DELETE", "/api/v1/jobs/"+id, nil, ""); w.Code != http.StatusForbidden {
		t.Fatalf("same-site DELETE with cookie: %d", w.Code)
	}
	delete(a.hdr, "Sec-Fetch-Site")
	delete(a.hdr, "Origin")
	if n := len(a.listIDs(t)); n != before {
		t.Fatalf("jobs %d -> %d", before, n)
	}
}

func headerFixture(t *testing.T, mod func(*config.Config)) sessFixture {
	return sessionFixture(t, func(c *config.Config) {
		c.Sessions = config.SessionsHeader
		c.SessionHeader = "X-Forwarded-User"
		c.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("fd00::/8")}
		if mod != nil {
			mod(c)
		}
	})
}

func TestHeaderModeTrustRules(t *testing.T) {
	f := headerFixture(t, nil)
	alice := newClient(f.h, "10.0.0.2:1")
	alice.hdr["X-Forwarded-User"] = "alice"
	bob := newClient(f.h, "[fd00::2]:1")
	bob.hdr["X-Forwarded-User"] = "bob"
	id := alice.createID(t)
	if len(alice.cookies) != 0 {
		t.Fatal("header mode set a cookie")
	}
	if ids := bob.listIDs(t); len(ids) != 0 {
		t.Fatalf("bob sees %v", ids)
	}
	if w := bob.get("/api/v1/jobs/" + id); w.Code != 404 {
		t.Fatalf("bob get: %d", w.Code)
	}
	if ids := alice.listIDs(t); len(ids) != 1 {
		t.Fatalf("alice sees %v", ids)
	}

	// spoofed header from an untrusted peer: ignored, so no identity -> 401
	spoof := newClient(f.h, "192.0.2.66:1")
	spoof.hdr["X-Forwarded-User"] = "alice"
	for _, req := range []struct{ method, path string }{{"GET", "/api/v1/jobs"}, {"GET", "/api/v1/jobs/" + id}, {"DELETE", "/api/v1/jobs/" + id}, {"GET", "/api/v1/jobs/" + id + "/events"}, {"GET", "/api/v1/quota"}} {
		w := spoof.do(req.method, req.path, nil, "")
		if w.Code != http.StatusUnauthorized {
			t.Errorf("spoofed %s %s: %d", req.method, req.path, w.Code)
		}
		problemOf(t, w)
	}
	if w := spoof.upload(t, 4); w.Code != http.StatusUnauthorized {
		t.Fatalf("spoofed create: %d", w.Code)
	}
	// trusted peer, but no / empty / several / invalid values
	noUser := newClient(f.h, "10.0.0.2:1")
	if w := noUser.get("/api/v1/jobs"); w.Code != http.StatusUnauthorized {
		t.Fatalf("no header: %d", w.Code)
	}
	noUser.hdr["X-Forwarded-User"] = "bad\x01user"
	if w := noUser.get("/api/v1/jobs"); w.Code != http.StatusUnauthorized {
		t.Fatalf("control char: %d", w.Code)
	}
	r := httptest.NewRequest("GET", "/api/v1/jobs", nil)
	r.RemoteAddr = "10.0.0.2:1"
	r.Header.Add("X-Forwarded-User", "alice")
	r.Header.Add("X-Forwarded-User", "bob")
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("two user headers: %d", w.Code)
	}
	// the presets list does not need an identity
	if w := spoof.get("/api/v1/presets"); w.Code != 200 {
		t.Fatalf("presets: %d", w.Code)
	}
	if !strings.Contains(f.log.String(), `"reason":"no_user_header"`) {
		t.Fatal("401 not audited")
	}
}

func retryAfter(t *testing.T, w *httptest.ResponseRecorder) int {
	t.Helper()
	n, err := strconv.Atoi(w.Header().Get("Retry-After"))
	if err != nil || n < 1 {
		t.Fatalf("Retry-After %q", w.Header().Get("Retry-After"))
	}
	return n
}

func TestQuotaActiveJobsAndBytesPerOwner(t *testing.T) {
	f := sessionFixture(t, func(c *config.Config) {
		c.Quota.ActiveJobsPerOwner = 3
		c.Quota.QueuedBytesPerOwner = 1000
	})
	a := newClient(f.h, "192.0.2.1:1")
	a.createID(t)
	a.createID(t)
	a.createID(t)
	w := a.upload(t, 4)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("third active job: %d %s", w.Code, w.Body)
	}
	retryAfter(t, w)
	if p := problemOf(t, w); !strings.Contains(p["detail"].(string), "unfinished jobs") {
		t.Fatalf("detail %v", p)
	}
	if n := len(inFiles(t, f.fixture)); n != 3 {
		t.Fatalf("refused upload stored: %d files", n)
	}
	// another session is not affected
	b := newClient(f.h, "192.0.2.2:1")
	b.createID(t)
	// a single file larger than the byte quota: 413
	if w := b.upload(t, 1001); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("file over the byte quota: %d", w.Code)
	}
	// bytes: 600 queued + 600 > 1000
	if w := b.upload(t, 600); w.Code != http.StatusAccepted {
		t.Fatalf("600: %d", w.Code)
	}
	w = newClientWith(b).upload(t, 600)
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "volume") {
		t.Fatalf("over the byte quota: %d %s", w.Code, w.Body)
	}
	if log := f.log.String(); !strings.Contains(log, `"status":429,"reason":"quota active_jobs"`) || !strings.Contains(log, `"remote_ip":"192.0.2.1","owner":"`) {
		t.Fatal("quota rejection not audited")
	}
}

// newClientWith returns a client sharing c's cookies (same session).
func newClientWith(c *client) *client {
	n := newClient(c.h, c.remote)
	for k, v := range c.cookies {
		n.cookies[k] = v
	}
	return n
}

// Parallel multipart uploads of one session: exactly the quota succeeds, the rest get 429, and no
// refused upload leaves its bytes behind (whether refused before or after the body was stored).
func TestQuotaRaceParallelUploads(t *testing.T) {
	f := sessionFixture(t, func(c *config.Config) { c.Quota.ActiveJobsPerOwner = 3 })
	a := newClient(f.h, "192.0.2.1:1")
	a.get("/api/v1/presets")
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := newClientWith(a).upload(t, 2048)
			mu.Lock()
			codes[w.Code]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if codes[202] != 3 || codes[429] != 13 {
		t.Fatalf("codes %v", codes)
	}
	if n := len(inFiles(t, f.fixture)); n != 3 {
		t.Fatalf("%d input files remain", n)
	}
}

func TestQuotaJobsPerHourAndQuotaEndpoint(t *testing.T) {
	f := sessionFixture(t, func(c *config.Config) {
		c.Quota.JobsPerHourPerOwner = 2
		c.Quota.ActiveJobsPerOwner = 5
	})
	a := newClient(f.h, "192.0.2.1:1")
	w := a.get("/api/v1/quota")
	if w.Code != 200 || len(a.cookies) != 1 {
		t.Fatalf("quota: %d cookies %d", w.Code, len(a.cookies))
	}
	id := a.createID(t)
	if w := a.do("DELETE", "/api/v1/jobs/"+id, nil, ""); w.Code != 204 {
		t.Fatal(w.Code)
	}
	a.createID(t)
	w = a.upload(t, 4)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("third job in an hour: %d", w.Code)
	}
	if n := retryAfter(t, w); n < 3500 || n > 3600 {
		t.Fatalf("Retry-After %d", n)
	}
	var q struct {
		Sessions string
		Owner    bool
		Limits   map[string]int64
		Usage    map[string]int64
		Resets   *int64 `json:"jobs_per_hour_resets_in_seconds"`
	}
	w = a.get("/api/v1/quota")
	if err := json.Unmarshal(w.Body.Bytes(), &q); err != nil {
		t.Fatal(err)
	}
	if q.Sessions != "cookie" || !q.Owner || q.Limits["jobs_per_hour"] != 2 || q.Limits["active_jobs"] != 5 ||
		q.Usage["jobs_per_hour"] != 2 || q.Usage["active_jobs"] != 1 || q.Usage["queued_bytes"] != 4 || q.Resets == nil || *q.Resets < 3500 {
		t.Fatalf("quota %s", w.Body)
	}
	// another client sees its own (empty) usage
	b := newClient(f.h, "192.0.2.2:1")
	if w := b.get("/api/v1/quota"); !strings.Contains(w.Body.String(), `"usage":{"active_jobs":0,"queued_bytes":0,"jobs_per_hour":0`) {
		t.Fatalf("b quota %s", w.Body)
	}
}

func TestQuotaPerIPAndSessionMinting(t *testing.T) {
	f := sessionFixture(t, func(c *config.Config) {
		c.Quota.ActiveJobsPerIP = 2
		c.Quota.SessionsPerHourPerIP = 3
	})
	// clearing cookies (a new client from the same address) does not reset the per-IP limit
	newClient(f.h, "192.0.2.1:1").createID(t)
	newClient(f.h, "192.0.2.1:2").createID(t)
	w := newClient(f.h, "192.0.2.1:3").upload(t, 4)
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "network address") {
		t.Fatalf("third job from one IP: %d %s", w.Code, w.Body)
	}
	// that was the third session from the address; the fourth is refused
	c := newClient(f.h, "192.0.2.1:4")
	if w := c.get("/api/v1/presets"); w.Code != 200 || len(c.cookies) != 0 {
		t.Fatalf("presets after the mint limit: %d cookies %v", w.Code, c.cookies)
	}
	w = c.upload(t, 4)
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "sessions") {
		t.Fatalf("create after the mint limit: %d %s", w.Code, w.Body)
	}
	retryAfter(t, w)
	if n := len(inFiles(t, f.fixture)); n != 2 {
		t.Fatalf("%d input files", n)
	}
	// IPv6: one /64 is one address for these limits
	newClient(f.h, "[2001:db8:1:2::1]:1").createID(t)
	newClient(f.h, "[2001:db8:1:2::2]:1").createID(t)
	if w := newClient(f.h, "[2001:db8:1:2::3]:1").upload(t, 4); w.Code != http.StatusTooManyRequests {
		t.Fatalf("third job from one /64: %d", w.Code)
	}
	if w := newClient(f.h, "[2001:db8:1:3::1]:1").upload(t, 4); w.Code != http.StatusAccepted {
		t.Fatalf("another /64: %d", w.Code)
	}
}

func TestPresignQuotaAndIsolation(t *testing.T) {
	f := newPresignFixture(t, 1000, func(c *config.Config) {
		c.Sessions = config.SessionsCookie
		c.SessionTTL = time.Hour
		c.Quota.ActiveJobsPerOwner = 1
		c.Quota.QueuedBytesPerOwner = 100
	})
	a, b := newClient(f.h, "192.0.2.1:1"), newClient(f.h, "192.0.2.2:1")
	w := a.do("POST", "/api/v1/jobs", strings.NewReader(`{"preset":"audio-wav","filename":"a.wav","size":4}`), "application/json")
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var v presignResp
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	if j, _ := f.store.Get(context.Background(), v.ID); j.Owner == nil || j.InputSize != 4 {
		t.Fatalf("presigned job %+v", j)
	}
	if w := a.do("POST", "/api/v1/jobs", strings.NewReader(`{"preset":"audio-wav","filename":"a.wav","size":4}`), "application/json"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("second presigned create: %d", w.Code)
	}
	if w := b.do("POST", "/api/v1/jobs", strings.NewReader(`{"preset":"audio-wav","filename":"a.wav","size":101}`), "application/json"); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("declared size over the byte quota: %d", w.Code)
	}
	f.fake.put("in/"+v.ID, 4)
	// B cannot complete A's upload, nor get a download URL
	if w := b.do("POST", "/api/v1/jobs/"+v.ID+"/complete", nil, ""); w.Code != 404 {
		t.Fatalf("B complete: %d", w.Code)
	}
	if w := a.do("POST", "/api/v1/jobs/"+v.ID+"/complete", nil, ""); w.Code != 200 {
		t.Fatalf("A complete: %d %s", w.Code, w.Body)
	}
	ctx := context.Background()
	if _, err := f.store.Claim(ctx, "w1", presets.IDs(presets.All())); err != nil {
		t.Fatal(err)
	}
	_ = f.store.SetRunning(ctx, v.ID, "w1", "pcm_s16le")
	f.fake.put("out/"+v.ID+".1.wav", 9)
	_ = f.store.Complete(ctx, v.ID, "w1", "out/"+v.ID+".1.wav")
	w = b.get("/api/v1/jobs/" + v.ID + "/output")
	if w.Code != 404 || w.Header().Get("Location") != "" || strings.Contains(w.Body.String(), fakeSig) {
		t.Fatalf("B output: %d %v", w.Code, w.Header())
	}
	if w := a.get("/api/v1/jobs/" + v.ID + "/output"); w.Code != http.StatusFound {
		t.Fatalf("A output: %d", w.Code)
	}
}

func TestSessionsOffQuotaEndpointAndMetrics(t *testing.T) {
	f := newFixtureWith(t, 1<<20, func(c *config.Config, d *api.Deps) { c.Quota.ActiveJobsPerIP = 1 })
	c := newClient(f.h, "192.0.2.1:1")
	if w := c.get("/api/v1/presets"); len(w.Header().Values("Set-Cookie")) != 0 {
		t.Fatal("sessions off set a cookie")
	}
	id := c.createID(t)
	if j, _ := f.store.Get(context.Background(), id); j.Owner != nil {
		t.Fatalf("sessions off: owner %x", j.Owner)
	}
	if w := newClient(f.h, "192.0.2.1:2").upload(t, 4); w.Code != http.StatusTooManyRequests {
		t.Fatalf("per-IP quota with sessions off: %d", w.Code)
	}
	if ids := newClient(f.h, "192.0.2.9:1").listIDs(t); len(ids) != 1 {
		t.Fatal("sessions off must list every job")
	}
	w := c.get("/api/v1/quota")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"sessions":"off","owner":false`) || !strings.Contains(w.Body.String(), `"active_jobs_per_ip":1}`) {
		t.Fatalf("quota: %s", w.Body)
	}
	// metrics: fixed vocabulary only, nothing per owner or per address
	m := c.get("/metrics").Body.String()
	for _, bad := range []string{"owner", "session", "192.0.2", id} {
		if strings.Contains(m, bad) {
			t.Errorf("metrics contain %q", bad)
		}
	}
}
