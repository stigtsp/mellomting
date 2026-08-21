package backend

import (
	"net/url"
	"strings"
	"testing"
)

// FuzzBaseURLValidation feeds arbitrary bytes to the backend base-URL
// validator (PLAN §80 "backend URL validation", PLAN §15.1). It must
// never panic; every input is either accepted (with a host and a usable
// scheme) or rejected with a clean error.
func FuzzBaseURLValidation(f *testing.F) {
	f.Add("http://127.0.0.1:8001")
	f.Add("https://example.com")
	f.Add("ftp://x")
	f.Add("http://")
	f.Add("http://user:pass@host/")
	f.Add("http://host/path")
	f.Add("http://host?q=1")
	f.Add("http://host#frag")
	f.Add(strings.Repeat("http://x/", 100))
	f.Add("http://\x7f")
	f.Fuzz(func(t *testing.T, raw string) {
		u, err := parseBaseURL(raw)
		if err != nil {
			return
		}
		if u.Hostname() == "" {
			t.Fatal("parseBaseURL accepted a URL with no host")
		}
		splitHostPort(u)
	})
}

// FuzzSplitHostPort feeds arbitrary bytes to the host/port splitter (PLAN
// §80 "backend URL validation" family, T-Q6). It must never panic and
// must always produce a port for a scheme it understands.
func FuzzSplitHostPort(f *testing.F) {
	f.Add("http://127.0.0.1:8001")
	f.Add("http://example.com")
	f.Add("https://example.com:443")
	f.Add("://x")
	f.Add(strings.Repeat("http://x:", 50))
	f.Fuzz(func(t *testing.T, raw string) {
		u, err := url.Parse(raw)
		if err != nil {
			return
		}
		_, port := splitHostPort(u)
		if port == "" {
			t.Fatal("splitHostPort returned an empty port")
		}
	})
}
