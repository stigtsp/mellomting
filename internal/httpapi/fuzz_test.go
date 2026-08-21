package httpapi

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"mellomting/internal/auth"
	"mellomting/internal/limiter"
)

// FuzzAuthorize feeds arbitrary bytes to the ingress Authorization /
// X-Api-Key parser (PLAN §80 "Authorization parser", PLAN §25). The
// invariants: no panic on any input (including the header slicing in the
// dual-header branch), and every failure is the same sanitized error —
// never a value that embeds the attempted key material.
func FuzzAuthorize(f *testing.F) {
	pepper := []byte("httpapi-fuzz-pepper-16")
	key, id, err := auth.Generate()
	if err != nil {
		f.Fatal(err)
	}
	uf := &auth.UsersFile{Version: 1, Keys: []auth.Key{
		{ID: id, Name: "f", SecretHash: auth.FormatHashValue(auth.Hash(pepper, key)), Enabled: true, Models: []string{"*"}},
	}}
	store, err := auth.NewStore(uf, pepper)
	if err != nil {
		f.Fatal(err)
	}
	authLog, err := limiter.NewBucket(1, 3)
	if err != nil {
		f.Fatal(err)
	}
	s := &Server{}
	s.store.Store(store)
	s.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	s.authLog = authLog

	f.Add("Bearer "+key, key)
	f.Add("", "")
	f.Add("Bearer", "x")
	f.Add("Basic abc", "abc")
	f.Add("Bearer a b", "x")
	f.Add("Bearer  ", " ")
	f.Add(strings.Repeat("Bearer x ", 200), strings.Repeat("y", 200))
	f.Add("Bearer mtk_AAAAAAAA_xxxxxxxxxxxxxxxxxxxxxxxx", "mtk_AAAAAAAA_xxxxxxxxxxxxxxxxxxxxxxxx")
	f.Fuzz(func(t *testing.T, authz, xkey string) {
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		r.Header.Set("Authorization", authz)
		r.Header.Set("X-Api-Key", xkey)
		_, err := s.authorize(r)
		if err != nil {
			// The client-facing error must never embed the raw key.
			if strings.Contains(err.Error(), "mtk_") {
				t.Fatalf("authorize error leaks key material: %q", err)
			}
		}
	})
}

// FuzzWriteErr feeds arbitrary bytes to the OpenAI-shaped error writer
// (PLAN §80 "OpenAI-style error rewriting", PLAN §72). The output must
// always be a valid JSON object with an error object; a hostile message
// must never corrupt the framing or crash the writer.
func FuzzWriteErr(f *testing.F) {
	f.Add("message", "type", "code")
	f.Add("", "", "")
	f.Add(strings.Repeat("x", 4096), "t", "c")
	f.Add("\"quoted\"", "type\"hdr", "code\nnewline")
	f.Fuzz(func(t *testing.T, msg, typ, code string) {
		w := httptest.NewRecorder()
		writeErr(w, 400, typ, code, msg)
		if w.Code != 400 {
			t.Fatalf("status = %d, want 400", w.Code)
		}
		if !strings.Contains(w.Body.String(), `"error"`) {
			t.Fatalf("error body is not OpenAI-shaped: %q", w.Body.String())
		}
	})
}
