package hostcheck_test

import (
	"testing"

	"github.com/ShifuRain/convertflow/internal/hostcheck"
)

func TestDefaults(t *testing.T) {
	m, err := hostcheck.Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		host string
		want bool
	}{
		{"localhost", true},
		{"localhost:8080", true},
		{"LOCALHOST:8080", true},
		{"localhost.", true},
		{"127.0.0.1", true},
		{"127.0.0.1:8080", true},
		{"10.1.2.3:80", true},
		{"[::1]", true},
		{"[::1]:8080", true},
		{"[fe80::1]:9", true},
		{"[2001:DB8::1]:9", true},
		{"::1", false}, // unbracketed IPv6 is not a valid Host
		{"[127.0.0.1]", false},
		{"[::1", false},
		{"[::1]x", false},
		{"[::1]:", false},
		{"[::1%eth0]:8080", false},
		{"evil.example", false},
		{"evil.example:8080", false},
		{"evil.example.", false},
		{"EVIL.example", false},
		{"", false},
		{" ", false},
		{"localhost ", false},
		{"local host", false},
		{":8080", false},
		{"localhost:", false},
		{"localhost:abc", false},
		{"localhost:99999", false},
		{"localhost:-1", false},
		{"localhost:+1", false},
		{"localhost:1:2", false},
		{"127.0.0.1@evil.example", false},
		{"evil.example@127.0.0.1", false},
		{"user:pw@127.0.0.1", false},
		{"127.0.0.1.evil.example", false},
		{"localhost.evil.example", false},
		{"evil.example/127.0.0.1", false},
		{"evil.example#127.0.0.1", false},
		{"evil.example?127.0.0.1", false},
		{"127.1", false},
		{"0x7f.0.0.1", false},
		{"2130706433", false},
		{"localhost\x00.evil.example", false},
		{"localhost\r\nX: y", false},
		{"lоcalhost", false}, // Cyrillic o
	}
	for _, c := range cases {
		if got := m.Allowed(c.host); got != c.want {
			t.Errorf("Allowed(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

func TestConfigured(t *testing.T) {
	m, err := hostcheck.Parse([]string{"Convert.Example.org", " files.example:8443 ", "[2001:db8::5]:9000"})
	if err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]bool{
		"convert.example.org":      true,
		"CONVERT.EXAMPLE.ORG:8080": true, // no port in the entry: any port
		"convert.example.org.":     true,
		"files.example:8443":       true,
		"files.example":            false, // entry pins the port
		"files.example:80":         false,
		"[2001:db8::5]:9000":       true, // IP literals are always allowed anyway
		"other.example":            false,
		"127.0.0.1:8080":           true, // defaults stay allowed (health probes)
		"localhost":                true,
	} {
		if got := m.Allowed(host); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestStar(t *testing.T) {
	m, err := hostcheck.Parse([]string{"*"})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"evil.example", "", "x y"} {
		if !m.Allowed(h) {
			t.Errorf("star must allow %q", h)
		}
	}
}

func TestParseRejectsBadEntries(t *testing.T) {
	for _, l := range [][]string{{"a b"}, {"http://x"}, {"x@y"}, {"x:port"}, {"::1"}, {"*", "x.example"}, {"x.example", "*"}, {"[bad]"}} {
		if _, err := hostcheck.Parse(l); err == nil {
			t.Errorf("Parse(%v) should fail", l)
		}
	}
}
