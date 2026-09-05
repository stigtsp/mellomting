package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// FuzzShallowParse feeds arbitrary bytes to the shallow request-body
// parser (PLAN §80 "public model extraction"). It must never panic, and a
// successful parse must always yield a non-empty public model name.
func FuzzShallowParse(f *testing.F) {
	f.Add([]byte(`{"model":"m","stream":true}`))
	f.Add([]byte(`{"model":123}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`[1,2]`))
	f.Add([]byte(`{"model":"m","model":"m2"}`))
	f.Add([]byte(strings.Repeat(`{"model":"m",`, 100) + `}`))
	f.Add([]byte(`{"model":"` + strings.Repeat("x", 4096) + `"}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		_, model, _, err := shallowParse(body)
		if err != nil {
			return
		}
		if model == "" {
			t.Fatal("shallowParse returned empty model without error")
		}
	})
}

// FuzzStringField feeds arbitrary bytes to the top-level string-field
// extractor (PLAN §80 "previous_response_id extraction" / "max-token
// field extraction": any top-level scalar field). It must never panic.
func FuzzStringField(f *testing.F) {
	f.Add([]byte(`{"previous_response_id":"r1","max_tokens":5}`), "previous_response_id")
	f.Add([]byte(`{"max_tokens":5}`), "max_tokens")
	f.Add([]byte(`{}`), "")
	f.Add([]byte(`{"a":"b"}`), "a")
	f.Add([]byte(`null`), "model")
	f.Add([]byte(`{"a":1.5e300}`), "a")
	f.Add([]byte(`{"a":"`+strings.Repeat("x", 4096)+`"}`), "a")
	f.Fuzz(func(t *testing.T, body []byte, field string) {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			return
		}
		stringField(fields, field)
	})
}

// FuzzTokenLimit feeds arbitrary bytes to the numeric output-limit
// extractor (PLAN §80 "max-token field extraction"). It must never panic
// and must classify every input as absent, capped, or invalid.
func FuzzTokenLimit(f *testing.F) {
	f.Add([]byte(`{"max_tokens":5}`), "max_tokens")
	f.Add([]byte(`{"max_tokens":"x"}`), "max_tokens")
	f.Add([]byte(`{"max_tokens":-1}`), "max_tokens")
	f.Add([]byte(`{"max_tokens":9223372036854775807}`), "max_tokens")
	f.Add([]byte(`{"max_tokens":1e400}`), "max_tokens")
	f.Add([]byte(`{"max_tokens":0}`), "max_tokens")
	f.Fuzz(func(t *testing.T, raw []byte, name string) {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return
		}
		tokenLimit(fields, name, 100)
	})
}

// FuzzRewriteModel is a round-trip property for the model rewrite (PLAN
// §80, PLAN §13): whenever shallowParse succeeds, rewriteModel must
// produce valid JSON that keeps every non-model field and replaces the
// model with the upstream name. Invalid inputs (including `null`) must
// never panic.
func FuzzRewriteModel(f *testing.F) {
	f.Add([]byte(`{"model":"m","stream":true,"extra":[1,2]}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"model":123}`))
	f.Add([]byte(`{"model":"m","model":"m2"}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		orig, _, _, perr := shallowParse(body)
		if perr != nil {
			return
		}
		out, err := encodeOutbound(orig, "upstream-X")
		if err != nil {
			t.Fatalf("encodeOutbound failed on shallow-parsed body: %v", err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(out, &fields); err != nil {
			t.Fatalf("rewriteModel output is not JSON: %v", err)
		}
		if len(fields) == 0 {
			t.Fatal("rewriteModel dropped every field")
		}
		var m string
		if err := json.Unmarshal(fields["model"], &m); err != nil || m != "upstream-X" {
			t.Fatalf("model not rewritten to upstream name: %q err=%v", m, err)
		}
	})
}

// FuzzResponseIDExtraction feeds arbitrary bytes to the Responses
// response-ID extractors (PLAN §80 "Responses response-ID extraction"):
// the non-stream top-level id and the streaming response.created payload.
// It must never panic.
func FuzzResponseIDExtraction(f *testing.F) {
	f.Add([]byte(`{"id":"resp_1","object":"response"}`))
	f.Add([]byte(`{"type":"response.created","response":{"id":"resp_2"}}`))
	f.Add([]byte(`{"response":{"id":"resp_3"}}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`[1,2,3]`))
	f.Add([]byte(`{"id":"` + strings.Repeat("x", 4096) + `"}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		_ = responseIDFromData(string(body))
		_ = topLevelID(body)
	})
}

// FuzzIsValidResponseID feeds arbitrary bytes to the response-ID alphabet
// check (PLAN §80, T-M2). It must never panic and must stay total: every
// input is either safe or rejected, and a rejected ID is never a path
// separator.
func FuzzIsValidResponseID(f *testing.F) {
	f.Add("resp_1")
	f.Add("")
	f.Add("..")
	f.Add(strings.Repeat("a", 300))
	f.Add("resp.1")
	f.Add("a/b")
	f.Add("resp id")
	f.Fuzz(func(t *testing.T, id string) {
		if !isValidResponseID(id) {
			return
		}
		if strings.ContainsAny(id, "/.") || id == "" || len(id) > 256 {
			t.Fatalf("unsafe id accepted: %q", id)
		}
	})
}

// FuzzIsUsageOnlyChunk feeds arbitrary bytes to the stream usage-chunk
// detector (PLAN §80 "streaming usage extraction"). It must never panic.
func FuzzIsUsageOnlyChunk(f *testing.F) {
	f.Add(`{"usage":{"total_tokens":5}}`)
	f.Add(`data: {"usage":{"total_tokens":5}}`)
	f.Add("")
	f.Add("[DONE]")
	f.Add(strings.Repeat("x", 4096))
	f.Fuzz(func(t *testing.T, data string) {
		isUsageOnlyChunk(data)
	})
}

// FuzzPassthroughHeaders feeds arbitrary header values to the backend
// header allow-list (PLAN §80 "header filtering", PLAN §18). The security
// invariant: only the allow-listed end-to-end headers (User-Agent,
// Accept) may ever reach the backend; client auth/identity headers must
// never appear in the returned map.
func FuzzPassthroughHeaders(f *testing.F) {
	f.Add("ua", "acc", "auth", "xkey")
	f.Add("", "", "", "")
	f.Add(strings.Repeat("x", 4096), "", "secret", "secret")
	f.Add("ua\r\nX-Evil: 1", "a", "b", "c")
	f.Fuzz(func(t *testing.T, ua, acc, auth, xkey string) {
		r := &http.Request{Header: http.Header{
			"User-Agent":          {ua},
			"Accept":              {acc},
			"Authorization":       {"Bearer " + auth},
			"X-Api-Key":           {xkey},
			"Proxy-Authorization": {"Basic " + xkey},
			"X-Forwarded-For":     {"1.2.3.4"},
		}}
		out := passthroughHeaders(r)
		for name := range out {
			if name != "User-Agent" && name != "Accept" {
				t.Fatalf("header %q leaked to the backend", name)
			}
		}
	})
}
