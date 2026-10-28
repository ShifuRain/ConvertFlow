package session_test

import (
	"bytes"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/ShifuRain/convertflow/internal/session"
)

func TestTokenRoundTrip(t *testing.T) {
	tok, key, err := session.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 43 || len(key) != 32 {
		t.Fatalf("token %d chars, key %d bytes", len(tok), len(key))
	}
	got, ok := session.TokenKey(tok)
	if !ok || !bytes.Equal(got, key) {
		t.Fatal("token does not map to its key")
	}
	tok2, key2, _ := session.NewToken()
	if tok2 == tok || bytes.Equal(key, key2) {
		t.Fatal("tokens repeat")
	}
	if strings.Contains(string(key), tok) {
		t.Fatal("key contains the token")
	}
}

func TestTokenKeyRejectsForeignValues(t *testing.T) {
	tok, _, _ := session.NewToken()
	for _, v := range []string{"", "abc", tok + "A", tok[:42], strings.Repeat("!", 43), tok[:42] + "=",
		strings.Repeat("A", 42) + "B" /* non-canonical trailing bits */} {
		if _, ok := session.TokenKey(v); ok {
			t.Errorf("accepted %q", v)
		}
	}
}

func TestKeysAreNamespaced(t *testing.T) {
	tok, key, _ := session.NewToken()
	if bytes.Equal(session.HeaderKey(tok), key) {
		t.Fatal("a proxy user named like a token maps to the token's key")
	}
	if session.Prefix(key) == "" || len(session.Prefix(key)) != 8 {
		t.Fatal("prefix")
	}
}

func TestValidUser(t *testing.T) {
	for v, want := range map[string]bool{"alice": true, "a@b.example": true, "": false, "a\nb": false,
		"a\x00": false, strings.Repeat("x", 256): true, strings.Repeat("x", 257): false, "del\x7f": false} {
		if session.ValidUser(v) != want {
			t.Errorf("%q: want %v", v, want)
		}
	}
}

func pfx(s ...string) []netip.Prefix {
	var out []netip.Prefix
	for _, v := range s {
		out = append(out, netip.MustParsePrefix(v))
	}
	return out
}

func TestClientIP(t *testing.T) {
	trusted := pfx("10.0.0.0/8", "fd00::/8")
	cases := []struct {
		name, remote string
		xff          []string
		want         string
	}{
		{"direct", "192.0.2.1:5", nil, "192.0.2.1"},
		{"spoofed xff from untrusted peer is ignored", "192.0.2.1:5", []string{"203.0.113.9"}, "192.0.2.1"},
		{"trusted proxy", "10.1.1.1:5", []string{"203.0.113.9"}, "203.0.113.9"},
		{"chain: rightmost untrusted wins", "10.1.1.1:5", []string{"198.51.100.1, 203.0.113.9, 10.2.2.2"}, "203.0.113.9"},
		{"client-forged left part ignored", "10.1.1.1:5", []string{"1.1.1.1", "203.0.113.9"}, "203.0.113.9"},
		{"all trusted", "10.1.1.1:5", []string{"10.3.3.3"}, "10.3.3.3"},
		{"no header", "10.1.1.1:5", nil, "10.1.1.1"},
		{"malformed stops at last trusted", "10.1.1.1:5", []string{"203.0.113.9, garbage"}, "10.1.1.1"},
		{"ipv6 peer and client", "[fd00::1]:5", []string{"2001:db8::7"}, "2001:db8::7"},
		{"mapped v4 peer", "[::ffff:10.1.1.1]:5", []string{"203.0.113.9"}, "203.0.113.9"},
		{"zone rejected", "10.1.1.1:5", []string{"fe80::1%eth0"}, "10.1.1.1"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		for _, v := range c.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := session.ClientIP(r, trusted).String(); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
	// without trusted proxies X-Forwarded-For is never read
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.1.1.1:5"
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := session.ClientIP(r, nil).String(); got != "10.1.1.1" {
		t.Fatalf("got %s", got)
	}
}

func TestIPKeyGroupsIPv6By64(t *testing.T) {
	a := session.IPKey(netip.MustParseAddr("2001:db8:1:2::1"))
	b := session.IPKey(netip.MustParseAddr("2001:db8:1:2:ffff::9"))
	c := session.IPKey(netip.MustParseAddr("2001:db8:1:3::1"))
	if !bytes.Equal(a, b) || bytes.Equal(a, c) {
		t.Fatal("IPv6 must be keyed by /64")
	}
	v4a := session.IPKey(netip.MustParseAddr("192.0.2.1"))
	v4b := session.IPKey(netip.MustParseAddr("192.0.2.2"))
	if bytes.Equal(v4a, v4b) || !bytes.Equal(v4a, session.IPKey(netip.MustParseAddr("::ffff:192.0.2.1"))) {
		t.Fatal("IPv4 keys")
	}
}

func TestForwardedTLS(t *testing.T) {
	trusted := pfx("10.0.0.0/8")
	mk := func(remote string, proto ...string) bool {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		for _, p := range proto {
			r.Header.Add("X-Forwarded-Proto", p)
		}
		return session.ForwardedTLS(r, trusted)
	}
	if !mk("10.0.0.1:1", "https") || !mk("10.0.0.1:1", "HTTPS") {
		t.Fatal("trusted https not believed")
	}
	if mk("192.0.2.1:1", "https") || mk("10.0.0.1:1", "http") || mk("10.0.0.1:1") || mk("10.0.0.1:1", "https", "http") || mk("10.0.0.1:1", "https, http") {
		t.Fatal("untrusted or ambiguous proto believed")
	}
}
