package agent_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mellomting/internal/auth"
	"mellomting/internal/config"
)

// vllmLike advertises one model the way vLLM does, including fields the
// edge must never forward.
func vllmLike(t *testing.T, id string, maxLen int) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"object":"model","created":1,"owned_by":"vllm",`+
			`"root":"/home/ubuntu/models/%s","parent":null,"permission":[{"id":"perm"}],"max_model_len":%d}]}`, id, id, maxLen)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestAgentModelMetadata(t *testing.T) {
	e := newEdge(t, config.Agents{}, map[string]auth.Key{
		"gpu":   {Agent: "lab"},
		"alice": {Models: []string{"lab/*"}},
	})
	e.runAgent(t, "gpu", vllmLike(t, "qwen", 32768).URL)
	waitFor(t, "model to register", func() bool { return e.router.Has("lab/qwen") })

	models := func() map[string]map[string]any {
		code, body := e.do(t, "GET", "/v1/models", "alice", "")
		if code != 200 {
			t.Fatalf("models: %d %s", code, body)
		}
		for _, leak := range []string{"/home/ubuntu", "root", "permission", "parent", "vllm"} {
			if strings.Contains(body, leak) {
				t.Fatalf("/v1/models leaks %q: %s", leak, body)
			}
		}
		var list struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal([]byte(body), &list); err != nil {
			t.Fatal(err)
		}
		out := map[string]map[string]any{}
		for _, m := range list.Data {
			out[m["id"].(string)] = m
		}
		return out
	}

	m := models()["lab/qwen"]
	if m["context_length"] != float64(32768) || m["owned_by"] != "mellomting" || m["object"] != "model" {
		t.Fatalf("metadata: %v", m)
	}

	// A second replica with a smaller window: the advertised window is
	// the minimum, since a request may land on either.
	e.runAgent(t, "gpu", vllmLike(t, "qwen", 8192).URL)
	waitFor(t, "second replica", func() bool {
		_, ok := e.registry.Client("agent-gpu-2")
		return ok
	})
	waitFor(t, "minimum window", func() bool { return models()["lab/qwen"]["context_length"] == float64(8192) })
}
