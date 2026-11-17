package discovery

import (
	"strings"
	"testing"
)

func TestParseModelCardsContextWindow(t *testing.T) {
	ok := `{"object":"list","data":[
		{"object":"model","id":"vllm","max_model_len":32768,"root":"/srv/models/x","owned_by":"vllm"},
		{"object":"model","id":"openrouter","context_length":131072},
		{"object":"model","id":"both","max_model_len":4096,"context_length":8192},
		{"object":"model","id":"null-falls-back","max_model_len":null,"context_length":2048},
		{"object":"model","id":"none"}]}`
	cards, err := ParseModelCards(strings.NewReader(ok), 1<<20, 16)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"vllm": 32768, "openrouter": 131072, "both": 4096, "null-falls-back": 2048, "none": 0}
	if len(cards) != len(want) {
		t.Fatalf("got %d cards", len(cards))
	}
	for i, c := range cards {
		if i > 0 && cards[i-1].ID > c.ID {
			t.Fatal("cards must be sorted by ID")
		}
		if c.ContextLength != want[c.ID] {
			t.Errorf("%s: window %d, want %d", c.ID, c.ContextLength, want[c.ID])
		}
	}
	for _, bad := range []string{"4096.0", `"4096"`, "-1", "0", "2147483648", "4e3", "true"} {
		doc := `{"object":"list","data":[{"object":"model","id":"m","max_model_len":` + bad + `}]}`
		if _, err := ParseModelCards(strings.NewReader(doc), 1<<20, 16); err == nil {
			t.Errorf("window %s accepted; a malformed window must fail the response", bad)
		}
	}
}
