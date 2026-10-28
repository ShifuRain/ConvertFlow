// Package config parses ConvertFlow's environment-based configuration.
package config

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ShifuRain/convertflow/internal/hostcheck"
)

type Config struct {
	ListenAddr     string
	DatabaseURL    string
	DataDir        string
	Flavor         string // "lgpl" or "full"
	TLSCertFile    string
	TLSKeyFile     string
	VAAPIDevice    string
	MaxUploadBytes int64
	MaxPixels      int64
	MaxDurationSec float64
	MaxStreams     int
	MaxAttempts    int
	MaxOutputBytes int64         // worker kills a conversion whose output exceeds this
	JobTimeout     time.Duration // worker wall-clock limit for one FFmpeg run
	Retention      time.Duration
	StaleAfter     time.Duration
	MaxQueueAge    time.Duration // queued jobs older than this are failed by the janitor
	WorkerPoll     time.Duration
	// AllowedHosts extends the default Host allow-list (localhost and IP literals); a single "*"
	// disables the check. It defends against DNS rebinding.
	AllowedHosts []string

	// Upload resource bounds (the service has no authentication, so these cap what anyone on the
	// network can consume).
	MaxConcurrentUploads int   // simultaneous in-flight upload requests; excess get 503
	MaxQueuedJobs        int   // queued+probing+running jobs; excess uploads get 503
	MinFreeBytes         int64 // free space required on the data volume; below it uploads get 507
	MaxSSEStreams        int   // simultaneous progress streams; excess get 503

	// JobStallTimeout makes the worker kill and fail a conversion whose FFmpeg reports no
	// progress and stays essentially idle for this long. Zero disables the watchdog.
	JobStallTimeout time.Duration
	// WorkerOnce and WorkerIdleExit select the worker's ephemeral modes (flags --once and
	// --idle-exit override them): Once processes at most one job; IdleExit makes the worker exit
	// after that long without a claimable job (0: never; once mode defaults to
	// DefaultOnceIdleExit).
	WorkerOnce     bool
	WorkerIdleExit time.Duration
	// WorkerShutdownTimeout bounds how long a stopping worker spends releasing an interrupted
	// job (and the other writes after a run).
	WorkerShutdownTimeout time.Duration
	// WorkersOnDemand declares that workers are started on demand (scale to zero), so the API and
	// janitor must not require a live worker: presets are offered from recent worker
	// registrations (OnDemandCapabilityWindow) and queued jobs expire purely by age.
	WorkersOnDemand bool

	// Sessions selects per-session job ownership: SessionsOff (every client sees every job, the
	// single-user default), SessionsCookie (anonymous server-issued session cookie) or
	// SessionsHeader (owner id from a header set by a trusted reverse proxy).
	Sessions string
	// SessionTTL is the idle lifetime of a cookie session (it is refreshed while in use).
	SessionTTL time.Duration
	// SessionHeader names the header carrying the user id in header mode.
	SessionHeader string
	// TrustedProxies are the peers whose X-Forwarded-For, X-Forwarded-Proto and SessionHeader
	// values are believed. Required in header mode; optional otherwise (client IP for per-IP
	// quotas, the cookie's Secure flag behind a TLS-terminating proxy).
	TrustedProxies []netip.Prefix
	// Quota holds the per-owner and per-IP abuse limits (0 = unlimited).
	Quota Quota

	// Storage selects where job inputs and outputs live: StorageLocal (DataDir) or StorageS3.
	Storage string
	// S3 is the object store configuration; only read (and validated) when Storage is StorageS3.
	S3 S3Config
}

// Session modes.
const (
	SessionsOff    = "off"
	SessionsCookie = "cookie"
	SessionsHeader = "header"
)

// Quota limits job creation per owner (session or proxy user) and per client IP. Zero means
// unlimited. They are abuse limits, not a security boundary.
type Quota struct {
	ActiveJobsPerOwner  int   // uploading+queued+probing+running jobs of one owner
	QueuedBytesPerOwner int64 // summed input bytes of one owner's active jobs
	JobsPerHourPerOwner int   // jobs one owner created in the trailing hour
	ActiveJobsPerIP     int   // active jobs created from one client IP (IPv6: one /64)
	// SessionsPerHourPerIP limits how many cookie sessions one client IP may obtain per hour.
	SessionsPerHourPerIP int
}

// DefaultSessionsPerHourPerIP bounds session minting in cookie mode unless configured otherwise.
const DefaultSessionsPerHourPerIP = 60

// Storage backends.
const (
	StorageLocal = "local"
	StorageS3    = "s3"
)

// DefaultOnceIdleExit is how long a --once worker waits for a claimable job when no idle exit is
// configured: a worker started for a job that is not yet visible (an autoscaler reacting to a row
// being committed) still finds it.
const DefaultOnceIdleExit = 30 * time.Second

// OnDemandCapabilityWindow is how far back worker registrations count when workers run on demand:
// a scaled-to-zero deployment has no live worker, so the presets offered are those a worker that
// registered within this window could run.
const OnDemandCapabilityWindow = 7 * 24 * time.Hour

// MinStallTimeout is the smallest accepted non-zero CONVERTFLOW_JOB_STALL_TIMEOUT.
const MinStallTimeout = 10 * time.Second

// MaxPresignTTL caps how long a presigned upload or download URL stays valid.
const MaxPresignTTL = time.Hour

// S3Config configures the S3-compatible object store.
type S3Config struct {
	Endpoint       string        // host:port the api and worker connect to
	PublicEndpoint string        // scheme://host[:port] browsers reach; presigned URLs are signed for it
	Bucket         string        // must exist; it is never created
	Region         string        // signing region
	UseSSL         bool          // TLS to Endpoint
	PathStyle      bool          // path-style (true) or virtual-host-style bucket addressing
	PresignTTL     time.Duration // lifetime of presigned URLs (1s..1h)
	AccessKey      Secret
	SecretKey      Secret
}

// Secret holds a credential. Its String and GoString forms are redacted, so a config printed with
// %v, %+v or %#v (for example in a log line or a test failure) never shows the value.
type Secret string

func (Secret) String() string   { return "[redacted]" }
func (Secret) GoString() string { return `"[redacted]"` }

// Value returns the secret itself.
func (s Secret) Value() string { return string(s) }

// bucketName follows the S3 bucket naming rules (3-63 chars, lowercase letters, digits, dots and
// hyphens, starting and ending with a letter or digit).
var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

func Load(getenv func(string) string, readFile func(string) ([]byte, error)) (Config, error) {
	var firstErr error
	fail := func(format string, a ...any) {
		if firstErr == nil {
			firstErr = fmt.Errorf(format, a...)
		}
	}
	str := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	num := func(key string, def int64) int64 {
		v := getenv(key)
		if v == "" {
			return def
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			fail("%s must be a positive integer, got %q", key, v)
			return def
		}
		return n
	}
	dur := func(key string, def time.Duration) time.Duration {
		v := getenv(key)
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			fail("%s must be a positive duration, got %q", key, v)
			return def
		}
		return d
	}

	c := Config{
		ListenAddr:     str("CONVERTFLOW_LISTEN", "127.0.0.1:8080"),
		DataDir:        str("CONVERTFLOW_DATA_DIR", "/data"),
		Flavor:         str("CONVERTFLOW_FLAVOR", "lgpl"),
		TLSCertFile:    getenv("CONVERTFLOW_TLS_CERT_FILE"),
		TLSKeyFile:     getenv("CONVERTFLOW_TLS_KEY_FILE"),
		VAAPIDevice:    str("CONVERTFLOW_VAAPI_DEVICE", "/dev/dri/renderD128"),
		MaxUploadBytes: num("CONVERTFLOW_MAX_UPLOAD_BYTES", 8<<30),
		MaxPixels:      num("CONVERTFLOW_MAX_PIXELS", 8192*4320),
		MaxDurationSec: float64(num("CONVERTFLOW_MAX_DURATION_SEC", 6*3600)),
		MaxStreams:     int(num("CONVERTFLOW_MAX_STREAMS", 16)),
		MaxAttempts:    int(num("CONVERTFLOW_MAX_ATTEMPTS", 3)),
		MaxOutputBytes: num("CONVERTFLOW_MAX_OUTPUT_BYTES", 16<<30),
		JobTimeout:     dur("CONVERTFLOW_JOB_TIMEOUT", 4*time.Hour),
		Retention:      dur("CONVERTFLOW_RETENTION", 24*time.Hour),
		StaleAfter:     dur("CONVERTFLOW_STALE_AFTER", 60*time.Second),
		MaxQueueAge:    dur("CONVERTFLOW_MAX_QUEUE_AGE", 24*time.Hour),
		WorkerPoll:     dur("CONVERTFLOW_WORKER_POLL", 2*time.Second),
	}
	c.MaxConcurrentUploads = int(num("CONVERTFLOW_MAX_CONCURRENT_UPLOADS", 4))
	c.MaxQueuedJobs = int(num("CONVERTFLOW_MAX_QUEUED_JOBS", 100))
	c.MaxSSEStreams = int(num("CONVERTFLOW_MAX_SSE_STREAMS", 64))
	// Default: room for one maximum-size upload plus a similarly sized output.
	c.MinFreeBytes = num("CONVERTFLOW_MIN_FREE_BYTES", 2*c.MaxUploadBytes)
	if v := getenv("CONVERTFLOW_ALLOWED_HOSTS"); v != "" {
		for _, h := range strings.Split(v, ",") {
			if h = strings.TrimSpace(h); h != "" {
				c.AllowedHosts = append(c.AllowedHosts, h)
			}
		}
		if _, err := hostcheck.Parse(c.AllowedHosts); err != nil {
			fail("CONVERTFLOW_ALLOWED_HOSTS: %v", err)
		}
	}
	if c.Flavor != "lgpl" && c.Flavor != "full" {
		fail("CONVERTFLOW_FLAVOR must be \"lgpl\" or \"full\", got %q", c.Flavor)
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		fail("CONVERTFLOW_TLS_CERT_FILE and CONVERTFLOW_TLS_KEY_FILE must be set together")
	}
	c.DatabaseURL = getenv("CONVERTFLOW_DATABASE_URL")
	if f := getenv("CONVERTFLOW_DATABASE_URL_FILE"); f != "" {
		b, err := readFile(f)
		if err != nil {
			fail("reading CONVERTFLOW_DATABASE_URL_FILE: %v", err)
		} else {
			c.DatabaseURL = strings.TrimSpace(string(b))
		}
	}
	if c.DatabaseURL == "" {
		fail("CONVERTFLOW_DATABASE_URL or CONVERTFLOW_DATABASE_URL_FILE is required")
	}

	boolean := func(key string, def bool) bool {
		v := getenv(key)
		if v == "" {
			return def
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			fail("%s must be true or false, got %q", key, v)
			return def
		}
		return b
	}
	c.WorkerOnce = boolean("CONVERTFLOW_WORKER_ONCE", false)
	c.WorkersOnDemand = boolean("CONVERTFLOW_WORKERS_ON_DEMAND", false)
	if v := getenv("CONVERTFLOW_WORKER_IDLE_EXIT"); v != "" {
		c.WorkerIdleExit = dur("CONVERTFLOW_WORKER_IDLE_EXIT", 0)
	}
	c.WorkerShutdownTimeout = dur("CONVERTFLOW_WORKER_SHUTDOWN_TIMEOUT", 10*time.Second)
	if c.WorkerShutdownTimeout > 5*time.Minute {
		fail("CONVERTFLOW_WORKER_SHUTDOWN_TIMEOUT must be at most 5m, got %s", c.WorkerShutdownTimeout)
	}
	c.JobStallTimeout = 10 * time.Minute
	if v := getenv("CONVERTFLOW_JOB_STALL_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		switch {
		case err != nil || d < 0:
			fail("CONVERTFLOW_JOB_STALL_TIMEOUT must be a duration (0 disables), got %q", v)
		case d > 0 && d < MinStallTimeout:
			fail("CONVERTFLOW_JOB_STALL_TIMEOUT must be 0 or at least %s, got %s", MinStallTimeout, d)
		default:
			c.JobStallTimeout = d
		}
	}

	nonNeg := func(key string) int64 {
		v := getenv(key)
		if v == "" {
			return 0
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			fail("%s must be a non-negative integer (0 = unlimited), got %q", key, v)
			return 0
		}
		return n
	}
	c.Sessions = str("CONVERTFLOW_SESSIONS", SessionsOff)
	c.SessionTTL = dur("CONVERTFLOW_SESSION_TTL", 30*24*time.Hour)
	c.SessionHeader = str("CONVERTFLOW_SESSION_HEADER", "X-Forwarded-User")
	if !headerName.MatchString(c.SessionHeader) {
		fail("CONVERTFLOW_SESSION_HEADER is not a valid header name: %q", c.SessionHeader)
	}
	if v := getenv("CONVERTFLOW_TRUSTED_PROXIES"); v != "" {
		for _, e := range strings.Split(v, ",") {
			if e = strings.TrimSpace(e); e == "" {
				continue
			}
			p, err := parsePrefix(e)
			if err != nil {
				fail("CONVERTFLOW_TRUSTED_PROXIES: %v", err)
				continue
			}
			c.TrustedProxies = append(c.TrustedProxies, p)
		}
	}
	c.Quota = Quota{
		ActiveJobsPerOwner:  int(nonNeg("CONVERTFLOW_QUOTA_ACTIVE_JOBS_PER_OWNER")),
		QueuedBytesPerOwner: nonNeg("CONVERTFLOW_QUOTA_QUEUED_BYTES_PER_OWNER"),
		JobsPerHourPerOwner: int(nonNeg("CONVERTFLOW_QUOTA_JOBS_PER_HOUR_PER_OWNER")),
		ActiveJobsPerIP:     int(nonNeg("CONVERTFLOW_QUOTA_ACTIVE_JOBS_PER_IP")),
	}
	c.Quota.SessionsPerHourPerIP = DefaultSessionsPerHourPerIP
	if getenv("CONVERTFLOW_QUOTA_SESSIONS_PER_HOUR_PER_IP") != "" {
		c.Quota.SessionsPerHourPerIP = int(nonNeg("CONVERTFLOW_QUOTA_SESSIONS_PER_HOUR_PER_IP"))
	}
	switch c.Sessions {
	case SessionsOff, SessionsCookie:
	case SessionsHeader:
		if len(c.TrustedProxies) == 0 {
			fail("CONVERTFLOW_TRUSTED_PROXIES is required when CONVERTFLOW_SESSIONS=header")
		}
	default:
		fail("CONVERTFLOW_SESSIONS must be %q, %q or %q, got %q", SessionsOff, SessionsCookie, SessionsHeader, c.Sessions)
	}
	if c.Sessions == SessionsOff && (c.Quota.ActiveJobsPerOwner > 0 || c.Quota.QueuedBytesPerOwner > 0 || c.Quota.JobsPerHourPerOwner > 0) {
		fail("the CONVERTFLOW_QUOTA_*_PER_OWNER limits need an owner: set CONVERTFLOW_SESSIONS to cookie or header")
	}

	secretFile := func(key string) Secret {
		f := getenv(key)
		if f == "" {
			fail("%s is required when CONVERTFLOW_STORAGE=s3", key)
			return ""
		}
		b, err := readFile(f)
		if err != nil {
			fail("reading %s: %v", key, err) // the path is the operator's; the content is never printed
			return ""
		}
		v := strings.TrimSpace(string(b))
		if v == "" {
			fail("%s names an empty file", key)
		}
		return Secret(v)
	}
	c.Storage = str("CONVERTFLOW_STORAGE", StorageLocal)
	// Credentials are only accepted from files: an environment value would be visible in the
	// process environment and in container inspect output.
	for _, k := range []string{"CONVERTFLOW_S3_ACCESS_KEY", "CONVERTFLOW_S3_SECRET_KEY"} {
		if getenv(k) != "" {
			fail("%s is not supported; put the value in a file and set %s_FILE", k, k)
		}
	}
	switch c.Storage {
	case StorageLocal:
	case StorageS3:
		s3 := S3Config{
			Endpoint:   getenv("CONVERTFLOW_S3_ENDPOINT"),
			Bucket:     getenv("CONVERTFLOW_S3_BUCKET"),
			Region:     str("CONVERTFLOW_S3_REGION", "us-east-1"),
			UseSSL:     boolean("CONVERTFLOW_S3_USE_SSL", true),
			PathStyle:  boolean("CONVERTFLOW_S3_PATH_STYLE", true),
			PresignTTL: dur("CONVERTFLOW_S3_PRESIGN_TTL", 15*time.Minute),
		}
		if err := validEndpoint(s3.Endpoint); err != nil {
			fail("CONVERTFLOW_S3_ENDPOINT: %v", err)
		}
		scheme := "https://"
		if !s3.UseSSL {
			scheme = "http://"
		}
		s3.PublicEndpoint = str("CONVERTFLOW_S3_PUBLIC_ENDPOINT", scheme+s3.Endpoint)
		if err := validPublicEndpoint(s3.PublicEndpoint); err != nil {
			fail("CONVERTFLOW_S3_PUBLIC_ENDPOINT: %v", err)
		}
		s3.PublicEndpoint = strings.TrimSuffix(s3.PublicEndpoint, "/")
		if !bucketName.MatchString(s3.Bucket) || strings.Contains(s3.Bucket, "..") {
			fail("CONVERTFLOW_S3_BUCKET must be a valid S3 bucket name, got %q", s3.Bucket)
		}
		if strings.ContainsAny(s3.Region, " /\\") {
			fail("CONVERTFLOW_S3_REGION is not a valid region name: %q", s3.Region)
		}
		if s3.PresignTTL > MaxPresignTTL || s3.PresignTTL < time.Second {
			fail("CONVERTFLOW_S3_PRESIGN_TTL must be between 1s and %s, got %s", MaxPresignTTL, s3.PresignTTL)
		}
		s3.AccessKey = secretFile("CONVERTFLOW_S3_ACCESS_KEY_FILE")
		s3.SecretKey = secretFile("CONVERTFLOW_S3_SECRET_KEY_FILE")
		c.S3 = s3
	default:
		fail("CONVERTFLOW_STORAGE must be %q or %q, got %q", StorageLocal, StorageS3, c.Storage)
	}
	return c, firstErr
}

// headerName is an HTTP field name (RFC 9110 token).
var headerName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]{1,64}$")

// parsePrefix accepts a CIDR prefix or a single IP address (a /32 or /128), and returns it masked.
func parsePrefix(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil || p.Addr().Zone() != "" || (p.Addr().Is4In6() && p.Bits() < 96) {
			return netip.Prefix{}, fmt.Errorf("invalid CIDR %q", s)
		}
		return netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-unmapBits(p.Addr())).Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil || a.Zone() != "" {
		return netip.Prefix{}, fmt.Errorf("invalid IP address %q", s)
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// unmapBits is 96 for an IPv4-mapped IPv6 address (::ffff:a.b.c.d/n becomes a.b.c.d/(n-96)).
func unmapBits(a netip.Addr) int {
	if a.Is4In6() {
		return 96
	}
	return 0
}

// validEndpoint accepts host:port or host (no scheme, path or credentials).
func validEndpoint(ep string) error {
	if ep == "" {
		return fmt.Errorf("required when CONVERTFLOW_STORAGE=s3")
	}
	if strings.Contains(ep, "://") || strings.ContainsAny(ep, "/@?# ") {
		return fmt.Errorf("must be host or host:port without scheme or path, got %q", ep)
	}
	host := ep
	if h, p, err := net.SplitHostPort(ep); err == nil {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("invalid port in %q", ep)
		}
		host = h
	} else if strings.Count(ep, ":") > 0 {
		return fmt.Errorf("must be host or host:port, got %q", ep)
	}
	if host == "" {
		return fmt.Errorf("missing host in %q", ep)
	}
	return nil
}

// validPublicEndpoint accepts an http(s) base URL without path, query, fragment or credentials.
func validPublicEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL %q", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("must start with http:// or https://, got %q", raw)
	}
	if u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return fmt.Errorf("must be scheme://host[:port] without path, query or credentials, got %q", raw)
	}
	return validEndpoint(u.Host)
}

// Absolutize makes every filesystem path of the configuration absolute, so later working-directory
// changes or relative comparisons (the janitor's inside-the-data-dir check) cannot be confused.
func (c *Config) Absolutize() error {
	for _, p := range []*string{&c.DataDir, &c.TLSCertFile, &c.TLSKeyFile} {
		if *p == "" {
			continue
		}
		abs, err := filepath.Abs(*p)
		if err != nil {
			return fmt.Errorf("resolving %q: %w", *p, err)
		}
		*p = abs
	}
	return nil
}
