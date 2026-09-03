package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// sourceServer is one inference server in the operator-facing source
// schema (D11). The url is the OpenAI-compatible base URL and api_key_file
// is an optional credential file (PLAN §17); the file value itself is never
// logged, only the path is configuration.
type sourceServer struct {
	URL        string `yaml:"url"`
	APIKeyFile string `yaml:"api_key_file"`
}

// sourceModel is one compact public model in the source schema (D11). The
// servers list names inference servers in replica order; upstream_model is
// an optional alias that defaults to the public map key (D12).
type sourceModel struct {
	Type          string      `yaml:"type"`
	Strategy      string      `yaml:"strategy"`
	Policy        ModelPolicy `yaml:"policy"`
	Servers       []string    `yaml:"servers"`
	UpstreamModel string      `yaml:"upstream_model"`
}

// sourceDoc is the operator-facing source configuration (D11): the sole
// accepted source form. The development-era backends: form and the shelved
// qualifiers: form are rejected as unknown fields by the strict decoder
// (PLAN §28, §88). The normalized runtime Config (D12) is produced from a
// separate unexported type, so no source-only state leaks into the public
// Config value.
type sourceDoc struct {
	Version    int                     `yaml:"version"`
	Server     Server                  `yaml:"server"`
	Auth       Auth                    `yaml:"auth"`
	Security   Security                `yaml:"security"`
	Logging    Logging                 `yaml:"logging"`
	Accounting Accounting              `yaml:"accounting"`
	Limits     Limits                  `yaml:"limits"`
	Shutdown   Shutdown                `yaml:"shutdown"`
	Responses  Responses               `yaml:"responses"`
	Retry      Retry                   `yaml:"retry"`
	Servers    map[string]sourceServer `yaml:"servers"`
	Models     map[string]sourceModel  `yaml:"models"`
}

// defaultBackendName derives the synthetic internal backend name for a
// (public model, server) pair (D12 step 3): "auto-" + server name + "-" +
// the first 12 lowercase hex characters of
// sha256(publicModel + "\x00" + serverName).
func defaultBackendName(publicModel, server string) string {
	sum := sha256.Sum256([]byte(publicModel + "\x00" + server))
	return "auto-" + server + "-" + hex.EncodeToString(sum[:])[:12]
}

// normalizeSource applies the deterministic D12 normalization: it turns
// the operator source document into the normalized runtime Config with
// synthetic internal backends. It fails closed on models without servers,
// on missing or duplicate server references, on servers no model
// references, and on generated-name collisions.
func normalizeSource(doc sourceDoc) (*Config, error) {
	return normalizeSourceWith(doc, defaultBackendName)
}

// normalizeSourceWith is the injectable normalization helper used to unit
// test the generated-name collision path without mutating package state.
func normalizeSourceWith(doc sourceDoc, backendName func(publicModel, server string) string) (*Config, error) {
	var errs []string
	cfg := &Config{
		Version:    doc.Version,
		Server:     doc.Server,
		Auth:       doc.Auth,
		Security:   doc.Security,
		Logging:    doc.Logging,
		Accounting: doc.Accounting,
		Limits:     doc.Limits,
		Shutdown:   doc.Shutdown,
		Responses:  doc.Responses,
		Retry:      doc.Retry,
		Backends:   make(map[string]Backend, len(doc.Servers)),
		Qualifiers: map[string]Qualifier{},
		Models:     make(map[string]Model, len(doc.Models)),
	}

	referenced := make(map[string]bool, len(doc.Servers))
	if len(doc.Servers) == 0 {
		errs = append(errs, "servers: at least one server is required")
	}
	if len(doc.Models) == 0 {
		errs = append(errs, "models: at least one model is required")
	}
	for _, publicName := range sortedModelKeys(doc.Models) {
		sm := doc.Models[publicName]
		if len(sm.Servers) == 0 {
			errs = append(errs, fmt.Sprintf("models.%s.servers: at least one server is required", publicName))
			continue
		}
		upstream := sm.UpstreamModel
		if upstream == "" {
			upstream = publicName
		}
		var refs []BackendRef
		seen := make(map[string]bool, len(sm.Servers))
		for _, server := range sm.Servers {
			if seen[server] {
				errs = append(errs, fmt.Sprintf("models.%s.servers: duplicate server %q", publicName, server))
				continue
			}
			seen[server] = true
			s, ok := doc.Servers[server]
			if !ok {
				errs = append(errs, fmt.Sprintf("models.%s.servers: unknown server %q", publicName, server))
				continue
			}
			name := backendName(publicName, server)
			if _, exists := cfg.Backends[name]; exists {
				errs = append(errs, fmt.Sprintf("generated backend name collision: %s", name))
				continue
			}
			cfg.Backends[name] = Backend{
				BaseURL:       s.URL,
				UpstreamModel: upstream,
				APIKeyFile:    s.APIKeyFile,
			}
			refs = append(refs, BackendRef{Name: name, Weight: 1})
			referenced[server] = true
		}
		cfg.Models[publicName] = Model{
			Type:     sm.Type,
			Strategy: sm.Strategy,
			Policy:   sm.Policy,
			Backends: refs,
		}
	}

	for server := range doc.Servers {
		if !referenced[server] {
			errs = append(errs, fmt.Sprintf("servers.%s: not referenced by any model", server))
		}
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return cfg, nil
}

// sortedModelKeys returns the public model names in UTF-8 bytewise sorted
// order (D12 step: deterministic normalization order).
func sortedModelKeys(models map[string]sourceModel) []string {
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// effectiveServer is the operator-facing projection of one inference
// server for `config show-effective` (D11). It carries the credential path
// but never the credential contents.
type effectiveServer struct {
	URL        string `yaml:"url"`
	APIKeyFile string `yaml:"api_key_file,omitempty"`
}

// effectiveModel is the fully defaulted operator-facing projection of one
// compact public model (D11, D12).
type effectiveModel struct {
	Type          string      `yaml:"type"`
	Strategy      string      `yaml:"strategy"`
	Policy        ModelPolicy `yaml:"policy,omitempty"`
	Servers       []string    `yaml:"servers"`
	UpstreamModel string      `yaml:"upstream_model,omitempty"`
}

// effectiveDoc is the fully defaulted, server-oriented source projection
// emitted by `config show-effective` (D11). It preserves operator server
// names and replica order, applies the D12 model defaults, and contains no
// synthetic internal backend names.
type effectiveDoc struct {
	Version    int                        `yaml:"version"`
	Server     Server                     `yaml:"server"`
	Auth       *Auth                      `yaml:"auth,omitempty"`
	Security   *Security                  `yaml:"security,omitempty"`
	Logging    *Logging                   `yaml:"logging,omitempty"`
	Accounting *Accounting                `yaml:"accounting,omitempty"`
	Limits     *Limits                    `yaml:"limits,omitempty"`
	Shutdown   *Shutdown                  `yaml:"shutdown,omitempty"`
	Responses  *Responses                 `yaml:"responses,omitempty"`
	Retry      *Retry                     `yaml:"retry,omitempty"`
	Servers    map[string]effectiveServer `yaml:"servers"`
	Models     map[string]effectiveModel  `yaml:"models"`
}

// effectiveSource builds the fully defaulted source projection from the
// original operator document and the normalized, defaulted runtime Config.
// It never reconstructs server names or replica order from synthetic
// backend names (D11).
func effectiveSource(doc sourceDoc, cfg *Config) effectiveDoc {
	servers := make(map[string]effectiveServer, len(doc.Servers))
	for name, s := range doc.Servers {
		servers[name] = effectiveServer{URL: s.URL, APIKeyFile: s.APIKeyFile}
	}

	models := make(map[string]effectiveModel, len(doc.Models))
	for name, sm := range doc.Models {
		m := cfg.Models[name]
		upstream := sm.UpstreamModel
		if upstream == "" {
			upstream = name
		}
		models[name] = effectiveModel{
			Type:          m.Type,
			Strategy:      m.Strategy,
			Policy:        m.Policy,
			Servers:       append([]string(nil), sm.Servers...),
			UpstreamModel: upstream,
		}
	}

	return effectiveDoc{
		Version:    cfg.Version,
		Server:     cfg.Server,
		Auth:       &cfg.Auth,
		Security:   &cfg.Security,
		Logging:    &cfg.Logging,
		Accounting: &cfg.Accounting,
		Limits:     &cfg.Limits,
		Shutdown:   &cfg.Shutdown,
		Responses:  &cfg.Responses,
		Retry:      &cfg.Retry,
		Servers:    servers,
		Models:     models,
	}
}

// MarshalEffectiveSource renders the effective source projection as
// deterministic YAML.
func (e effectiveDoc) MarshalEffectiveSource() ([]byte, error) {
	return yaml.Marshal(e)
}
