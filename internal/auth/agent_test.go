package auth

import (
	"strings"
	"testing"
	"time"
)

func TestAllowsNamespacePrefix(t *testing.T) {
	k := &Key{Models: []string{"lab/*", "exact"}}
	for model, want := range map[string]bool{
		"lab/m1":     true,
		"lab/org/m2": true,
		"lab/":       false, // nothing after the namespace
		"lab":        false,
		"labx/m1":    false,
		"exact":      true,
		"other/m1":   false,
	} {
		if got := k.Allows(model); got != want {
			t.Errorf("Allows(%q) = %v, want %v", model, got, want)
		}
	}
	// A bare "/*" is not a namespace grant.
	if (&Key{Models: []string{"/*"}}).Allows("/x") {
		t.Error(`"/*" must not grant anything`)
	}
}

func TestAgentKeyAllowsNoModel(t *testing.T) {
	k := &Key{Agent: "lab", Models: []string{"*"}}
	if k.Allows("lab/m1") || k.Allows("anything") {
		t.Fatal("an agent key must not allow any model")
	}
}

func TestValidateAgentKeys(t *testing.T) {
	base := func() Key {
		return Key{ID: "0123456789abcdef", Name: "gpu", Enabled: true, SecretHash: FormatHashValue(make([]byte, 32))}
	}
	cases := map[string]struct {
		mod  func(*Key)
		want string
	}{
		"agent key":          {func(k *Key) { k.Agent = "lab" }, ""},
		"wildcard namespace": {func(k *Key) { k.Agent = "*" }, ""},
		"agent with models":  {func(k *Key) { k.Agent = "lab"; k.Models = []string{"*"} }, "agent key has no models"},
		"bad namespace":      {func(k *Key) { k.Agent = "Lab/x" }, "invalid agent namespace"},
		"client no models":   {func(k *Key) {}, "models list must not be empty"},
	}
	for name, c := range cases {
		k := base()
		c.mod(&k)
		err := validateUsers(&UsersFile{Version: 1, Keys: []Key{k}})
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: got %v, want error containing %q", name, err, c.want)
		}
	}
}

func TestStoreActive(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	h := FormatHashValue(make([]byte, 32))
	uf := &UsersFile{Version: 1, Keys: []Key{
		{ID: "0000000000000001", Name: "a", Enabled: true, Agent: "lab", SecretHash: h},
		{ID: "0000000000000002", Name: "b", Enabled: false, Agent: "lab", SecretHash: h},
		{ID: "0000000000000003", Name: "c", Enabled: true, Agent: "lab", SecretHash: h, ExpiresAt: &past},
	}}
	s, err := NewStore(uf, []byte("0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	attached := func(id, agent, hash string) *Key { return &Key{ID: id, Agent: agent, SecretHash: hash} }
	other := FormatHashValue(append(make([]byte, 31), 1))
	for name, c := range map[string]struct {
		k    *Key
		want bool
	}{
		"unchanged":         {attached("0000000000000001", "lab", h), true},
		"disabled":          {attached("0000000000000002", "lab", h), false},
		"expired":           {attached("0000000000000003", "lab", h), false},
		"unknown":           {attached("00000000000000ff", "lab", h), false},
		"secret rotated":    {attached("0000000000000001", "lab", other), false},
		"namespace changed": {attached("0000000000000001", "exp", h), false},
	} {
		if got := s.Active(c.k); got != c.want {
			t.Errorf("%s: Active = %v, want %v", name, got, c.want)
		}
	}
}

func TestValidateRegister(t *testing.T) {
	base := func() Key {
		return Key{ID: "0123456789abcdef", Name: "gpu", Enabled: true, SecretHash: FormatHashValue(make([]byte, 32))}
	}
	reg := func(pairs ...string) []AgentModel {
		var out []AgentModel
		for i := 0; i < len(pairs); i += 2 {
			out = append(out, AgentModel{ID: pairs[i], As: pairs[i+1]})
		}
		return out
	}
	cases := map[string]struct {
		mod  func(*Key)
		want string
	}{
		"allowlist":                    {func(k *Key) { k.Agent = "lab"; k.Register = reg("qwen3:32b", "", "org/m2", "") }, ""},
		"aliases under *":              {func(k *Key) { k.Agent = "*"; k.Register = reg("qwen3-coder:30b", "coder", "org/m2", "m2") }, ""},
		"slash id under * needs alias": {func(k *Key) { k.Agent = "*"; k.Register = reg("org/m2", "") }, "cannot register names containing /"},
		"alias with slash":             {func(k *Key) { k.Agent = "lab"; k.Register = reg("m1", "x/y") }, "no /"},
		"invalid id":                   {func(k *Key) { k.Agent = "lab"; k.Register = reg("has space", "") }, "register id"},
		"invalid alias":                {func(k *Key) { k.Agent = "lab"; k.Register = reg("m1", "a,b") }, "register alias"},
		"wildcard alias":               {func(k *Key) { k.Agent = "*"; k.Register = reg("m1", "*") }, "register alias"},
		"id twice":                     {func(k *Key) { k.Agent = "lab"; k.Register = reg("m1", "a", "m1", "b") }, "listed twice"},
		"alias collision":              {func(k *Key) { k.Agent = "lab"; k.Register = reg("m1", "x", "m2", "x") }, "both be served as"},
		"alias collides with id":       {func(k *Key) { k.Agent = "*"; k.Register = reg("m1", "", "m2", "m1") }, "both be served as"},
		"client key with register":     {func(k *Key) { k.Models = []string{"*"}; k.Register = reg("m1", "") }, "only an agent key"},
	}
	for name, c := range cases {
		k := base()
		c.mod(&k)
		err := validateUsers(&UsersFile{Version: 1, Keys: []Key{k}})
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: got %v, want error containing %q", name, err, c.want)
		}
	}
}

// Editing an agent key's register list re-scopes it: attached agents
// stop being active, re-attach and register under the new list.
func TestStoreActiveRegisterChange(t *testing.T) {
	h := FormatHashValue(make([]byte, 32))
	cur := []AgentModel{{ID: "m1", As: "coder"}}
	uf := &UsersFile{Version: 1, Keys: []Key{{ID: "0000000000000001", Name: "a", Enabled: true, Agent: "*", SecretHash: h, Register: cur}}}
	s, err := NewStore(uf, []byte("0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		reg  []AgentModel
		want bool
	}{
		"unchanged":     {[]AgentModel{{ID: "m1", As: "coder"}}, true},
		"alias changed": {[]AgentModel{{ID: "m1", As: "other"}}, false},
		"entry added":   {[]AgentModel{{ID: "m1", As: "coder"}, {ID: "m2"}}, false},
		"list removed":  {nil, false},
	} {
		k := &Key{ID: "0000000000000001", Agent: "*", SecretHash: h, Register: c.reg}
		if got := s.Active(k); got != c.want {
			t.Errorf("%s: Active = %v, want %v", name, got, c.want)
		}
	}
}
