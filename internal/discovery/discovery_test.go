package discovery

import (
	"bytes"
	"strings"
	"testing"
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
		maxBytes := int64(len(b))
		if maxBytes > 1024 {
			maxBytes = 1024
		}
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
