// Package session derives the owner of a request (an anonymous cookie session or a user id
// asserted by a trusted reverse proxy) and the client address used for per-IP limits.
//
// Owner keys are SHA-256 digests with a fixed namespace prefix per kind, so a cookie token and a
// proxy user id can never map to the same key. Raw tokens and user ids are never stored or
// logged; the database holds only the digests.
package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// CookieName is the session cookie.
const CookieName = "cf_session"

// CookiePath scopes the cookie to the API: the UI's static files never receive it.
const CookiePath = "/api/"

// tokenBytes is the token's entropy (256 bits).
const tokenBytes = 32

// tokenLen is the length of the base64url (unpadded) encoding of a token.
var tokenLen = base64.RawURLEncoding.EncodedLen(tokenBytes)

const (
	nsCookie = "convertflow/session/cookie\x00"
	nsHeader = "convertflow/session/header\x00"
	nsIP     = "convertflow/client-ip\x00"
	nsMint   = "convertflow/session-mint\x00"
)

func digest(ns string, v []byte) []byte {
	h := sha256.New()
	h.Write([]byte(ns))
	h.Write(v)
	return h.Sum(nil)
}

// NewToken returns a fresh random token (for the cookie) and its owner key (for the database).
func NewToken() (token string, key []byte, err error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, digest(nsCookie, []byte(token)), nil
}

// TokenKey returns the owner key of a cookie value, or false when the value cannot be a token
// this server issued (wrong length or alphabet). Only the key is ever looked up.
func TokenKey(token string) ([]byte, bool) {
	if len(token) != tokenLen {
		return nil, false
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(b) != tokenBytes || base64.RawURLEncoding.EncodeToString(b) != token {
		return nil, false
	}
	return digest(nsCookie, []byte(token)), true
}

// HeaderKey returns the owner key of a user id asserted by a trusted proxy.
func HeaderKey(user string) []byte { return digest(nsHeader, []byte(user)) }

// ValidUser reports whether a proxy-asserted user id is acceptable: 1..256 bytes, no control
// characters.
func ValidUser(v string) bool {
	if v == "" || len(v) > 256 {
		return false
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// Prefix is the short, non-reversible form of an owner key used in audit records (8 hex chars).
func Prefix(key []byte) string {
	if len(key) < 4 {
		return ""
	}
	return hex.EncodeToString(key[:4])
}

// IPKey groups a client address for per-IP limits: an IPv4 address on its own, an IPv6 address
// by its /64 (a single host usually controls a whole /64). The result is a digest, but the
// IPv4 space is small enough to enumerate: treat it as pseudonymous, not anonymous.
func IPKey(a netip.Addr) []byte {
	a = a.Unmap()
	var s string
	switch {
	case !a.IsValid():
		s = "invalid"
	case a.Is4():
		s = a.String()
	default:
		p, _ := a.WithZone("").Prefix(64)
		s = p.String()
	}
	return digest(nsIP, []byte(s))
}

// MintKey is the rate-limit key for session creation from one client IP key.
func MintKey(ipKey []byte) []byte { return digest(nsMint, ipKey) }

// Trusted reports whether a is inside one of the trusted proxy prefixes.
func Trusted(a netip.Addr, trusted []netip.Prefix) bool {
	a = a.Unmap()
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Peer returns the address of the TCP peer (RemoteAddr). An unparsable RemoteAddr yields the
// zero Addr.
func Peer(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return a.WithZone("").Unmap()
}

// FromTrustedProxy reports whether the request's TCP peer is a trusted proxy.
func FromTrustedProxy(r *http.Request, trusted []netip.Prefix) bool {
	return len(trusted) > 0 && Trusted(Peer(r), trusted)
}

// ClientIP returns the client address. The peer is the client unless it is a trusted proxy;
// then X-Forwarded-For is walked from the right (the entry the nearest proxy added) past every
// trusted hop, and the first untrusted entry is the client. A malformed entry stops the walk at
// the last trusted hop, so a forged header can never move the result past a hop we trust.
// Several X-Forwarded-For lines are read as one comma-separated list, in order.
func ClientIP(r *http.Request, trusted []netip.Prefix) netip.Addr {
	peer := Peer(r)
	if !peer.IsValid() || !Trusted(peer, trusted) {
		return peer
	}
	var hops []string
	for _, line := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(line, ",")...)
	}
	client := peer
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil || a.Zone() != "" {
			return client
		}
		client = a.Unmap()
		if !Trusted(client, trusted) {
			return client
		}
	}
	return client
}

// ForwardedTLS reports whether a trusted proxy says the client connection used TLS
// (X-Forwarded-Proto: https). Untrusted peers are never believed.
func ForwardedTLS(r *http.Request, trusted []netip.Prefix) bool {
	if !FromTrustedProxy(r, trusted) {
		return false
	}
	// One value only: a list means a chain we cannot attribute, and is not believed.
	v := r.Header.Values("X-Forwarded-Proto")
	return len(v) == 1 && strings.EqualFold(strings.TrimSpace(v[0]), "https")
}
