package discovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"mellomting/internal/backend"
	"mellomting/internal/config"
)

func parseString(t *testing.T, s string, maxBytes int64, maxModels int) []string {
	t.Helper()
	ids, err := ParseModels(strings.NewReader(s), maxBytes, maxModels)
	if err != nil {
		t.Fatalf("ParseModels: %v", err)
	}
	return ids
}

func parseStringErr(t *testing.T, s string, maxBytes int64, maxModels int, wantSubstr string) error {
	t.Helper()
	_, err := ParseModels(strings.NewReader(s), maxBytes, maxModels)
	if err == nil {
		t.Fatalf("ParseModels succeeded, want %q error", wantSubstr)
	}
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Fatalf("ParseModels error = %v, want substring %q", err, wantSubstr)
	}
	return err
}

func entry(id string) string {
	return `{"id":"` + id + `","object":"model"}`
}

func TestParseModelsValid(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "empty data",
			in:   `{"object":"list","data":[]}`,
			want: []string{},
		},
		{
			name: "single model",
			in:   `{"object":"list","data":[` + entry("qwen3.8-27b") + `]}`,
			want: []string{"qwen3.8-27b"},
		},
		{
			name: "sorted output",
			in:   `{"object":"list","data":[` + entry("b") + "," + entry("a") + "," + entry("c") + `]}`,
			want: []string{"a", "b", "c"},
		},
		{
			name: "unknown fields ignored",
			in: `{
				"object":"list",
				"unknown-top":42,
				"data":[` + `{"id":"m","object":"model","extra":true}` + `],
				"trailing":null
			}`,
			want: []string{"m"},
		},
		{
			name: "trailing whitespace",
			in:   "{\"object\":\"list\",\"data\":[]}\n\t",
			want: []string{},
		},
		{
			name: "max id length",
			in:   `{"object":"list","data":[` + entry(strings.Repeat("a", MaxModelIDBytes)) + `]}`,
			want: []string{strings.Repeat("a", MaxModelIDBytes)},
		},
		{
			name: "combining character allowed",
			in:   `{"object":"list","data":[` + entry(`e\u0301`) + `]}`,
			want: []string{"e\u0301"},
		},
		{
			name: "internal space allowed",
			in:   `{"object":"list","data":[` + entry("a b") + `]}`,
			want: []string{"a b"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseString(t, tc.in, 1<<20, 1024)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d IDs %v, want %v", len(got), got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestParseModelsRejects(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty input", in: "", want: "invalid JSON"},
		{name: "truncated top object", in: `{"object":"list"`, want: "invalid JSON"},
		{name: "truncated array", in: `{"object":"list","data":[]`, want: "invalid JSON"},
		{name: "top-level array", in: `[]`, want: "top-level value must be an object"},
		{name: "top-level scalar", in: `42`, want: "top-level value must be an object"},
		{name: "missing object", in: `{"data":[]}`, want: `object must be "list"`},
		{name: "wrong object", in: `{"object":"model","data":[]}`, want: `object must be "list"`},
		{name: "missing data", in: `{"object":"list"}`, want: "data is required"},
		{name: "null data", in: `{"object":"list","data":null}`, want: "data must be an array"},
		{name: "scalar data", in: `{"object":"list","data":1}`, want: "data must be an array"},
		{name: "entry scalar", in: `{"object":"list","data":[1]}`, want: "data entry must be a JSON object"},
		{name: "entry null", in: `{"object":"list","data":[null]}`, want: `object must be "model"`},
		{name: "wrong entry object", in: `{"object":"list","data":[` + entryX("m", "list") + `]}`, want: `object must be "model"`},
		{name: "empty id", in: `{"object":"list","data":[` + entry("") + `]}`, want: "model ID is invalid"},
		{name: "id too long", in: `{"object":"list","data":[` + entry(strings.Repeat("a", MaxModelIDBytes+1)) + `]}`, want: "model ID is invalid"},
		{name: "duplicate id", in: `{"object":"list","data":[` + entry("a") + "," + entry("a") + `]}`, want: "duplicate model ID"},
		{name: "second document", in: `{"object":"list","data":[]}{}`, want: "exactly one JSON document"},
		{name: "trailing garbage", in: `{"object":"list","data":[]} x`, want: "exactly one JSON document"},
		{name: "control character", in: `{"object":"list","data":[` + entry(`\u0000`) + `]}`, want: "model ID is invalid"},
		{name: "control character 0x1f", in: `{"object":"list","data":[` + entry(`\u001f`) + `]}`, want: "model ID is invalid"},
		{name: "bidi override", in: `{"object":"list","data":[` + entry(`\u202E`) + `]}`, want: "model ID is invalid"},
		{name: "zero width space", in: `{"object":"list","data":[` + entry(`\u200B`) + `]}`, want: "model ID is invalid"},
		{name: "zero width joiner", in: `{"object":"list","data":[` + entry(`\u200D`) + `]}`, want: "model ID is invalid"},
		{name: "word joiner format", in: `{"object":"list","data":[` + entry(`\u2060`) + `]}`, want: "model ID is invalid"},
		{name: "line separator", in: `{"object":"list","data":[` + entry(`a\u2028b`) + `]}`, want: "model ID is invalid"},
		{name: "paragraph separator", in: `{"object":"list","data":[` + entry(`a\u2029b`) + `]}`, want: "model ID is invalid"},
		{name: "leading ascii space", in: `{"object":"list","data":[` + entry(" a") + `]}`, want: "model ID is invalid"},
		{name: "trailing ascii space", in: `{"object":"list","data":[` + entry("a ") + `]}`, want: "model ID is invalid"},
		{name: "leading nbsp", in: `{"object":"list","data":[` + entry(`\u00A0a`) + `]}`, want: "model ID is invalid"},
		{name: "trailing em space", in: `{"object":"list","data":[` + entry(`a\u2003`) + `]}`, want: "model ID is invalid"},
		{name: "leading ideographic space", in: `{"object":"list","data":[` + entry(`\u3000a`) + `]}`, want: "model ID is invalid"},
		// A model ID that means something else once it reaches a key's
		// model ACL: "*" is the wildcard granting every model, and a
		// comma separates entries in `key create --models`. The server
		// answering this endpoint is not trusted yet, so it must not be
		// able to name one.
		{name: "acl wildcard", in: `{"object":"list","data":[` + entry("*") + `]}`, want: "model ID is invalid"},
		{name: "acl separator", in: `{"object":"list","data":[` + entry("a,*") + `]}`, want: "model ID is invalid"},
		{name: "duplicate object field", in: `{"object":"list","object":"list","data":[]}`, want: "duplicate object field"},
		{name: "duplicate data field", in: `{"object":"list","data":[],"data":[]}`, want: "duplicate data field"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parseStringErr(t, tc.in, 1<<20, 1024, tc.want)
		})
	}
}

func TestParseModelsRejectsInvalidUTF8(t *testing.T) {
	t.Parallel()
	in := []byte(`{"object":"list","data":[{"id":"`)
	in = append(in, 0xff)
	in = append(in, []byte(`","object":"model"}]}`)...)
	_, err := ParseModels(bytes.NewReader(in), 1<<20, 1024)
	if err == nil {
		t.Fatal("ParseModels accepted invalid UTF-8")
	}
	if !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("error = %v, want invalid marker", err)
	}
}

func TestParseModelsBounds(t *testing.T) {
	t.Run("maxBytes exact", func(t *testing.T) {
		t.Parallel()
		in := `{"object":"list","data":[]}`
		ids := parseString(t, in, int64(len(in)), 1024)
		if len(ids) != 0 {
			t.Fatalf("got %v, want empty", ids)
		}
	})

	t.Run("maxBytes exceeded", func(t *testing.T) {
		t.Parallel()
		in := `{"object":"list","data":[]}`
		_, err := ParseModels(strings.NewReader(in), int64(len(in))-1, 1024)
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("error = %v, want exceeds", err)
		}
	})

	t.Run("maxBytes zero", func(t *testing.T) {
		t.Parallel()
		_, err := ParseModels(strings.NewReader("{}"), 0, 1024)
		if err == nil || !strings.Contains(err.Error(), "maxBytes") {
			t.Fatalf("error = %v, want maxBytes", err)
		}
	})

	t.Run("maxModels zero", func(t *testing.T) {
		t.Parallel()
		_, err := ParseModels(strings.NewReader(`{"object":"list","data":[]}`), 1<<20, 0)
		if err == nil || !strings.Contains(err.Error(), "maxModels") {
			t.Fatalf("error = %v, want maxModels", err)
		}
	})

	t.Run("maxModels exact", func(t *testing.T) {
		t.Parallel()
		in := `{"object":"list","data":[` + entry("a") + "," + entry("b") + `]}`
		ids := parseString(t, in, 1<<20, 2)
		if len(ids) != 2 {
			t.Fatalf("got %v, want two IDs", ids)
		}
	})

	t.Run("maxModels exceeded", func(t *testing.T) {
		t.Parallel()
		in := `{"object":"list","data":[` + entry("a") + "," + entry("b") + "," + entry("c") + `]}`
		_, err := ParseModels(strings.NewReader(in), 1<<20, 2)
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("error = %v, want exceeds", err)
		}
	})
}

func entryX(id, object string) string {
	return `{"id":"` + id + `","object":"` + object + `"}`
}

func FuzzParseModels(f *testing.F) {
	f.Add([]byte(`{"object":"list","data":[{"id":"a","object":"model"}]}`))
	f.Add([]byte(`{"object":"list","data":[{"id":"a","object":"model"}]`))
	f.Add([]byte(`{"object":"list","data":[{"id":"a","object":"model"},{"id":"a","object":"model"}]}`))
	f.Add([]byte(`{"object":"list","data":[]}{}`))
	f.Add(bytes.Repeat([]byte("x"), 2000))

	f.Fuzz(func(t *testing.T, b []byte) {
		maxBytes := min(int64(len(b)), 1024)
		if maxBytes == 0 {
			maxBytes = 1
		}
		_, _ = ParseModels(bytes.NewReader(b), maxBytes, 8)
	})
}

func TestParseModelsErrorDoesNotEchoBody(t *testing.T) {
	t.Parallel()
	secret := "discovery-secret-marker"
	in := `{"object":"list","data":[{"id":"` + secret + strings.Repeat("a", MaxModelIDBytes) + `","object":"model","note":"` + secret + `"}]}`
	_, err := ParseModels(strings.NewReader(in), 1<<20, 1024)
	if err == nil {
		t.Fatal("ParseModels succeeded, want invalid ID error")
	}
	if !strings.Contains(err.Error(), "model ID is invalid") {
		t.Fatalf("error = %v, want model ID class", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error echoes response body: %v", err)
	}
}

func loopbackOptions() Options {
	return Options{Network: config.BackendNetwork{Mode: "loopback-only"}}
}

func jsonModels(ids ...string) string {
	if len(ids) == 0 {
		return `{"object":"list","data":[]}`
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = `{"id":"` + id + `","object":"model"}`
	}
	return `{"object":"list","data":[` + strings.Join(parts, ",") + `]}`
}

func TestFetchExactRequest(t *testing.T) {
	var gotMethod, gotPath, gotAccept string
	var headers http.Header
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAccept = r.Header.Get("Accept")
		headers = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, jsonModels("m"))
	}))
	t.Cleanup(ts.Close)

	ids, err := Fetch(context.Background(), Server{Name: "local", BaseURL: ts.URL}, loopbackOptions())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !reflect.DeepEqual(ids, []string{"m"}) {
		t.Fatalf("ids = %v", ids)
	}
	if gotMethod != http.MethodGet || gotPath != "/v1/models" || gotAccept != "application/json" {
		t.Fatalf("request = %s %s accept=%q", gotMethod, gotPath, gotAccept)
	}
	for _, h := range []string{"Authorization", "Proxy-Authorization", "X-Api-Key", "Forwarded", "X-Forwarded-For", "X-Forwarded-Host"} {
		if len(headers[http.CanonicalHeaderKey(h)]) != 0 {
			t.Fatalf("forbidden header %s forwarded", h)
		}
	}
}

func TestFetchBoundsAndTimeouts(t *testing.T) {
	t.Run("response too large", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("a"))
			_, _ = w.Write(bytes.Repeat([]byte("b"), MaxResponseBytes))
		}))
		t.Cleanup(ts.Close)
		_, err := Fetch(context.Background(), Server{Name: "local", BaseURL: ts.URL}, loopbackOptions())
		if !errors.Is(err, ErrTooLarge) {
			t.Fatalf("err = %v, want ErrTooLarge", err)
		}
	})

	t.Run("too many models", func(t *testing.T) {
		ids := make([]string, MaxModels+1)
		for i := range ids {
			ids[i] = fmt.Sprintf("m%d", i)
		}
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, jsonModels(ids...))
		}))
		t.Cleanup(ts.Close)
		_, err := Fetch(context.Background(), Server{Name: "local", BaseURL: ts.URL}, loopbackOptions())
		if !errors.Is(err, ErrTooLarge) {
			t.Fatalf("err = %v, want ErrTooLarge", err)
		}
	})

	t.Run("dial timeout", func(t *testing.T) {
		opts := loopbackOptions()
		opts.connectTimeout = time.Millisecond
		opts.newDialer = func(backend.Policy, time.Duration, backend.Resolver) func(context.Context, string, string) (net.Conn, error) {
			return func(context.Context, string, string) (net.Conn, error) {
				return nil, backend.ErrDialTimeout
			}
		}
		_, err := Fetch(context.Background(), Server{Name: "local", BaseURL: "http://127.0.0.1:1"}, opts)
		if !errors.Is(err, ErrDialTimeout) {
			t.Fatalf("err = %v, want ErrDialTimeout", err)
		}
	})

	t.Run("connect failure", func(t *testing.T) {
		opts := loopbackOptions()
		opts.newDialer = func(backend.Policy, time.Duration, backend.Resolver) func(context.Context, string, string) (net.Conn, error) {
			return func(context.Context, string, string) (net.Conn, error) {
				return nil, backend.ErrConnect
			}
		}
		_, err := Fetch(context.Background(), Server{Name: "local", BaseURL: "http://127.0.0.1:1"}, opts)
		if !errors.Is(err, ErrConnect) {
			t.Fatalf("err = %v, want ErrConnect", err)
		}
	})

	t.Run("header timeout", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(50 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, jsonModels())
		}))
		t.Cleanup(ts.Close)
		opts := loopbackOptions()
		opts.headerTimeout = 5 * time.Millisecond
		opts.totalTimeout = 200 * time.Millisecond
		_, err := Fetch(context.Background(), Server{Name: "local", BaseURL: ts.URL}, opts)
		if !errors.Is(err, ErrHeaderTimeout) {
			t.Fatalf("err = %v, want ErrHeaderTimeout", err)
		}
	})

	t.Run("total timeout", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(50 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, jsonModels())
		}))
		t.Cleanup(ts.Close)
		opts := loopbackOptions()
		opts.headerTimeout = 200 * time.Millisecond
		opts.totalTimeout = 5 * time.Millisecond
		_, err := Fetch(context.Background(), Server{Name: "local", BaseURL: ts.URL}, opts)
		if !errors.Is(err, ErrTimeout) {
			t.Fatalf("err = %v, want ErrTimeout", err)
		}
	})
}

func TestFetchHTTPBehavior(t *testing.T) {
	t.Run("redirect refused", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/other", http.StatusMovedPermanently)
		}))
		t.Cleanup(ts.Close)
		_, err := Fetch(context.Background(), Server{Name: "local", BaseURL: ts.URL}, loopbackOptions())
		if !errors.Is(err, ErrStatus) {
			t.Fatalf("err = %v, want ErrStatus", err)
		}
	})

	t.Run("non-200 sanitized", func(t *testing.T) {
		secret := "upstream-secret-marker"
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, secret)
		}))
		t.Cleanup(ts.Close)
		_, err := Fetch(context.Background(), Server{Name: "local", BaseURL: ts.URL}, loopbackOptions())
		if !errors.Is(err, ErrStatus) || !strings.Contains(err.Error(), "503") {
			t.Fatalf("err = %v, want sanitized 503", err)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error echoes body: %v", err)
		}
	})

	t.Run("content types", func(t *testing.T) {
		t.Run("absent", func(t *testing.T) {
			opts := loopbackOptions()
			opts.do = func(*http.Client, *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{},
					Body:       io.NopCloser(strings.NewReader(jsonModels("m"))),
				}, nil
			}
			ids, err := Fetch(context.Background(), Server{Name: "local", BaseURL: "http://127.0.0.1:1"}, opts)
			if err != nil || !reflect.DeepEqual(ids, []string{"m"}) {
				t.Fatalf("ids=%v err=%v", ids, err)
			}
		})

		cases := []struct {
			name string
			ct   string
			want error
		}{
			{name: "plain json", ct: "application/json"},
			{name: "parameterized json", ct: "application/json; charset=utf-8"},
			{name: "non-json", ct: "text/plain", want: ErrContentType},
			{name: "malformed", ct: "application/", want: ErrContentType},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if tc.ct != "" {
						w.Header().Set("Content-Type", tc.ct)
					}
					fmt.Fprint(w, jsonModels("m"))
				}))
				t.Cleanup(ts.Close)
				ids, err := Fetch(context.Background(), Server{Name: "local", BaseURL: ts.URL}, loopbackOptions())
				if tc.want == nil {
					if err != nil || !reflect.DeepEqual(ids, []string{"m"}) {
						t.Fatalf("ids=%v err=%v", ids, err)
					}
					return
				}
				if !errors.Is(err, tc.want) {
					t.Fatalf("err = %v, want %v", err, tc.want)
				}
			})
		}
	})

	t.Run("proxy environment ignored", func(t *testing.T) {
		t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
		t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
		t.Setenv("ALL_PROXY", "http://127.0.0.1:1")
		t.Setenv("http_proxy", "http://127.0.0.1:1")
		t.Setenv("https_proxy", "http://127.0.0.1:1")
		t.Setenv("all_proxy", "http://127.0.0.1:1")

		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, jsonModels("m"))
		}))
		t.Cleanup(ts.Close)
		ids, err := Fetch(context.Background(), Server{Name: "local", BaseURL: ts.URL}, loopbackOptions())
		if err != nil || !reflect.DeepEqual(ids, []string{"m"}) {
			t.Fatalf("ids=%v err=%v", ids, err)
		}
	})

	t.Run("direct policy dialer only path", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, jsonModels("m"))
		}))
		t.Cleanup(ts.Close)
		u, _ := url.Parse(ts.URL)

		opts := loopbackOptions()
		var dialed []string
		opts.newDialer = func(p backend.Policy, ct time.Duration, r backend.Resolver) func(context.Context, string, string) (net.Conn, error) {
			base := backend.DialContext(p, ct, r)
			return func(ctx context.Context, network, addr string) (net.Conn, error) {
				dialed = append(dialed, addr)
				return base(ctx, network, addr)
			}
		}
		if _, err := Fetch(context.Background(), Server{Name: "local", BaseURL: ts.URL}, opts); err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if !reflect.DeepEqual(dialed, []string{u.Host}) {
			t.Fatalf("dialed = %v, want %v", dialed, u.Host)
		}
	})

	t.Run("MPTCP-off constructor used by default", func(t *testing.T) {
		orig := newDialerFunc
		called := false
		newDialerFunc = func(p backend.Policy, ct time.Duration, r backend.Resolver) func(context.Context, string, string) (net.Conn, error) {
			called = true
			return orig(p, ct, r)
		}
		t.Cleanup(func() { newDialerFunc = orig })

		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, jsonModels("m"))
		}))
		t.Cleanup(ts.Close)
		if _, err := Fetch(context.Background(), Server{Name: "local", BaseURL: ts.URL}, loopbackOptions()); err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if !called {
			t.Fatal("Fetch did not use the production dialer constructor")
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := Fetch(ctx, Server{Name: "local", BaseURL: "http://127.0.0.1:1"}, loopbackOptions())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
}

func TestDerivePolicy(t *testing.T) {
	cases := []struct {
		name    string
		servers []Server
		want    config.BackendNetwork
		wantErr string
	}{
		{
			name:    "all IPv4 loopback",
			servers: []Server{{Name: "a", BaseURL: "http://127.0.0.1:8001"}, {Name: "b", BaseURL: "http://127.0.0.2:8002"}},
			want:    config.BackendNetwork{Mode: "loopback-only"},
		},
		{
			name:    "all IPv6 loopback",
			servers: []Server{{Name: "a", BaseURL: "http://[::1]:8001"}},
			want:    config.BackendNetwork{Mode: "loopback-only"},
		},
		{
			name:    "mixed IPv4 includes loopback CIDR",
			servers: []Server{{Name: "a", BaseURL: "http://127.0.0.1:8001"}, {Name: "b", BaseURL: "http://10.0.0.2:8002"}},
			want:    config.BackendNetwork{Mode: "allowed-cidrs", CIDRs: []string{"10.0.0.2/32", "127.0.0.1/32"}},
		},
		{
			name:    "IPv6 exact host",
			servers: []Server{{Name: "a", BaseURL: "http://[2001:db8::1]:8001"}},
			want:    config.BackendNetwork{Mode: "allowed-cidrs", CIDRs: []string{"2001:db8::1/128"}},
		},
		{
			name:    "duplicate CIDRs removed",
			servers: []Server{{Name: "a", BaseURL: "http://10.0.0.2:8001"}, {Name: "b", BaseURL: "http://10.0.0.2:8002"}},
			want:    config.BackendNetwork{Mode: "allowed-cidrs", CIDRs: []string{"10.0.0.2/32"}},
		},
		{
			name:    "hostname rejected",
			servers: []Server{{Name: "a", BaseURL: "http://vllm.internal:8001"}},
			wantErr: "literal IP host",
		},
		{
			name:    "invalid URL rejected",
			servers: []Server{{Name: "a", BaseURL: "ftp://127.0.0.1:8001"}},
			wantErr: "scheme",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := DerivePolicy(tc.servers)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("DerivePolicy: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestFetchPolicyEnforcement(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, jsonModels("m"))
	}))
	t.Cleanup(ts.Close)

	t.Run("allowed CIDR accepted", func(t *testing.T) {
		opts := Options{Network: config.BackendNetwork{Mode: "allowed-cidrs", CIDRs: []string{"127.0.0.1/32"}}}
		ids, err := Fetch(context.Background(), Server{Name: "local", BaseURL: ts.URL}, opts)
		if err != nil || !reflect.DeepEqual(ids, []string{"m"}) {
			t.Fatalf("ids=%v err=%v", ids, err)
		}
	})

	t.Run("disallowed CIDR rejected", func(t *testing.T) {
		opts := Options{Network: config.BackendNetwork{Mode: "allowed-cidrs", CIDRs: []string{"10.0.0.0/8"}}}
		_, err := Fetch(context.Background(), Server{Name: "local", BaseURL: ts.URL}, opts)
		if !errors.Is(err, ErrPolicy) {
			t.Fatalf("err = %v, want ErrPolicy", err)
		}
	})
}

func TestAggregate(t *testing.T) {
	ordered := []Server{{Name: "a"}, {Name: "b"}, {Name: "c"}}

	t.Run("overlapping and disjoint sets", func(t *testing.T) {
		t.Parallel()
		got, err := Aggregate(ordered, map[string][]string{
			"a": {"x", "y"},
			"b": {"y", "z"},
			"c": nil,
		})
		if err != nil {
			t.Fatalf("Aggregate: %v", err)
		}
		want := map[string][]string{
			"x": {"a"},
			"y": {"a", "b"},
			"z": {"b"},
		}
		if !reflect.DeepEqual(got.Models, want) {
			t.Fatalf("models = %v, want %v", got.Models, want)
		}
		if !reflect.DeepEqual(got.SortedModels(), []string{"x", "y", "z"}) {
			t.Fatalf("sorted = %v", got.SortedModels())
		}
	})

	t.Run("stable server order", func(t *testing.T) {
		t.Parallel()
		got, err := Aggregate(ordered, map[string][]string{
			"c": {"m"},
			"a": {"m"},
			"b": {"m"},
		})
		if err != nil {
			t.Fatalf("Aggregate: %v", err)
		}
		if !reflect.DeepEqual(got.Models["m"], []string{"a", "b", "c"}) {
			t.Fatalf("replicas = %v, want server argument order", got.Models["m"])
		}
	})

	t.Run("empty results", func(t *testing.T) {
		t.Parallel()
		got, err := Aggregate(ordered, map[string][]string{"a": nil, "b": nil, "c": nil})
		if err != nil {
			t.Fatalf("Aggregate: %v", err)
		}
		if len(got.Models) != 0 {
			t.Fatalf("models = %v, want empty", got.Models)
		}
	})

	t.Run("missing result", func(t *testing.T) {
		t.Parallel()
		_, err := Aggregate(ordered, map[string][]string{"a": nil, "c": nil})
		if err == nil || !strings.Contains(err.Error(), `missing result for server "b"`) {
			t.Fatalf("err = %v, want missing b", err)
		}
	})

	t.Run("unexpected result", func(t *testing.T) {
		t.Parallel()
		_, err := Aggregate(ordered, map[string][]string{"a": nil, "b": nil, "c": nil, "d": nil})
		if err == nil || !strings.Contains(err.Error(), `unexpected result for server "d"`) {
			t.Fatalf("err = %v, want unexpected d", err)
		}
	})

	t.Run("duplicate server name", func(t *testing.T) {
		t.Parallel()
		_, err := Aggregate([]Server{{Name: "a"}, {Name: "a"}}, map[string][]string{"a": nil})
		if err == nil || !strings.Contains(err.Error(), "duplicate server name") {
			t.Fatalf("err = %v, want duplicate server", err)
		}
	})

	t.Run("empty server name", func(t *testing.T) {
		t.Parallel()
		_, err := Aggregate([]Server{{Name: ""}}, map[string][]string{"": nil})
		if err == nil || !strings.Contains(err.Error(), "server name is required") {
			t.Fatalf("err = %v, want server name required", err)
		}
	})

	t.Run("too many servers", func(t *testing.T) {
		t.Parallel()
		servers := make([]Server, MaxServers+1)
		discovered := make(map[string][]string, len(servers))
		for i := range servers {
			servers[i] = Server{Name: fmt.Sprintf("s%d", i)}
			discovered[servers[i].Name] = nil
		}
		_, err := Aggregate(servers, discovered)
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("exceeds %d servers", MaxServers)) {
			t.Fatalf("err = %v, want server bound", err)
		}
	})

	t.Run("server model bound", func(t *testing.T) {
		t.Parallel()
		ids := make([]string, MaxModels+1)
		for i := range ids {
			ids[i] = fmt.Sprintf("m%d", i)
		}
		_, err := Aggregate([]Server{{Name: "a"}}, map[string][]string{"a": ids})
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("exceeds %d models", MaxModels)) {
			t.Fatalf("err = %v, want model bound", err)
		}
	})

	t.Run("global unique model bound", func(t *testing.T) {
		t.Parallel()
		ordered := []Server{{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"}, {Name: "e"}}
		discovered := make(map[string][]string, len(ordered))
		for i, s := range ordered {
			ids := make([]string, 205)
			for j := range ids {
				ids[j] = fmt.Sprintf("s%d-m%d", i, j)
			}
			discovered[s.Name] = ids
		}
		_, err := Aggregate(ordered, discovered)
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("exceeds %d unique models", MaxUniqueModels)) {
			t.Fatalf("err = %v, want unique model bound", err)
		}
	})

	t.Run("duplicate model within server", func(t *testing.T) {
		t.Parallel()
		_, err := Aggregate([]Server{{Name: "a"}}, map[string][]string{"a": {"m", "m"}})
		if err == nil || !strings.Contains(err.Error(), "duplicate model") {
			t.Fatalf("err = %v, want duplicate model", err)
		}
	})

	t.Run("empty model ID", func(t *testing.T) {
		t.Parallel()
		_, err := Aggregate([]Server{{Name: "a"}}, map[string][]string{"a": {""}})
		if err == nil || !strings.Contains(err.Error(), "empty model ID") {
			t.Fatalf("err = %v, want empty model ID", err)
		}
	})
}
