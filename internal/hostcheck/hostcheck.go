// Package hostcheck implements the Host header allow-list that defends against DNS rebinding.
//
// A page on attacker.example can make a victim's browser resolve attacker.example to 127.0.0.1 (or
// a LAN address) after the page loaded; the browser then treats requests to the service as
// same-origin and may read responses. The browser still sends "Host: attacker.example", so the
// service refuses every request whose Host it does not recognise.
//
// Always allowed: "localhost" and any IP literal (IPv4, or bracketed IPv6), with or without a port.
// An attacker cannot make a browser send an IP-literal Host that resolves elsewhere, and loopback
// health probes use one. Everything else must be listed explicitly. A single "*" disables the check.
package hostcheck

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

type entry struct {
	host string // lower-case, without brackets or trailing dot
	port string // "" = any port
}

// Matcher decides whether a request Host is allowed. The zero value allows the defaults only.
type Matcher struct {
	any     bool
	entries []entry
}

// Parse builds a Matcher from configured entries (host or host:port, [v6] or [v6]:port, or "*").
func Parse(list []string) (Matcher, error) {
	var m Matcher
	for _, raw := range list {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if raw == "*" {
			if len(list) > 1 {
				return Matcher{}, fmt.Errorf("%q must be the only entry", "*")
			}
			m.any = true
			continue
		}
		host, port, ok := split(raw)
		if !ok || host == "" {
			return Matcher{}, fmt.Errorf("invalid host entry %q (want host, host:port, [v6] or [v6]:port)", raw)
		}
		m.entries = append(m.entries, entry{host, port})
	}
	return m, nil
}

// Allowed reports whether the Host header value of a request is acceptable.
func (m Matcher) Allowed(hostHeader string) bool {
	if m.any {
		return true
	}
	host, port, ok := split(hostHeader)
	if !ok || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	for _, e := range m.entries {
		if e.host == host && (e.port == "" || e.port == port) {
			return true
		}
	}
	return false
}

// split separates a Host value into a normalised hostname (lower-case, brackets and one trailing
// dot removed, but an IP literal keeps its exact text) and a port ("" when absent). It reports
// false for anything that is not a plain host[:port]: whitespace, userinfo, paths, zones, an
// unbracketed IPv6 address, an empty or non-numeric port.
func split(s string) (host, port string, ok bool) {
	if s == "" || len(s) > 255 {
		return "", "", false
	}
	for _, c := range []byte(s) {
		if c <= ' ' || c >= 0x7f || strings.IndexByte("@/\\?#%", c) >= 0 {
			return "", "", false
		}
	}
	rest := s
	bracketed := false
	if s[0] == '[' {
		end := strings.IndexByte(s, ']')
		if end < 0 {
			return "", "", false
		}
		host, rest, bracketed = s[1:end], s[end+1:], true
		if a, err := netip.ParseAddr(host); err != nil || !a.Is6() {
			return "", "", false
		}
	} else {
		i := strings.IndexByte(s, ':')
		if i >= 0 {
			host, rest = s[:i], s[i:]
		} else {
			host, rest = s, ""
		}
		if strings.ContainsAny(host, "[]:") {
			return "", "", false
		}
	}
	if rest != "" {
		if rest[0] != ':' || len(rest) < 2 || len(rest) > 6 {
			return "", "", false
		}
		n, err := strconv.Atoi(rest[1:])
		if err != nil || n < 0 || n > 65535 || strings.ContainsAny(rest[1:], "+-") {
			return "", "", false
		}
		port = strconv.Itoa(n)
	}
	host = strings.ToLower(host)
	if !bracketed {
		if _, err := netip.ParseAddr(host); err != nil { // a name: tolerate one trailing dot
			host = strings.TrimSuffix(host, ".")
		}
	}
	return host, port, true
}
