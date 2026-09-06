package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Load reads the configuration file at path, applies defaults, and
// validates it. Any failure is an error; there is no partial state.
func Load(path string) (*Config, error) {
	data, err := readBounded(path)
	if err != nil {
		return nil, withPath(path, err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, withPath(path, err)
	}
	return cfg, nil
}

// withPath names the file an error is about, unless the error already
// does. os returns "open /etc/x.yaml: no such file or directory", and
// prefixing that again reads as two different files.
func withPath(path string, err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return err
	}
	return fmt.Errorf("%s: %w", path, err)
}

func readBounded(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("exceeds maximum size of %d bytes", MaxFileBytes)
	}
	return data, nil
}

// Parse accepts raw configuration bytes, applies defaults, and validates
// the result.
//
// Decoding is strict per PLAN §28: YAML aliases/anchors, custom tags,
// unknown fields, and duplicate keys are all rejected, and multi-document
// YAML (T-M7) is rejected so configuration is never silently discarded.
// The document is decoded through the operator-facing source schema (D11)
// and deterministically normalized (D12) into the runtime Config before
// defaults and strict internal validation are applied.
func Parse(data []byte) (*Config, error) {
	cfg, _, err := parse(data)
	return cfg, err
}

// ParseEffective accepts raw configuration bytes and returns both the
// normalized runtime Config and the fully defaulted server-oriented source
// projection (D11). The projection is the canonical output of
// `config show-effective`; it never exposes synthetic internal backend
// names.
func ParseEffective(data []byte) (*Config, []byte, error) {
	cfg, doc, err := parse(data)
	if err != nil {
		return nil, nil, err
	}
	out, err := doc.MarshalEffectiveSource()
	if err != nil {
		return nil, nil, fmt.Errorf("render effective configuration: %w", err)
	}
	return cfg, out, nil
}

// LoadEffective reads the configuration file at path and returns both the
// normalized runtime Config and the fully defaulted source projection.
func LoadEffective(path string) (*Config, []byte, error) {
	data, err := readBounded(path)
	if err != nil {
		return nil, nil, withPath(path, err)
	}
	cfg, out, err := ParseEffective(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, out, nil
}

func parse(data []byte) (*Config, effectiveDoc, error) {
	if err := checkShape(data); err != nil {
		return nil, effectiveDoc{}, wrapYAML(err)
	}
	if err := CheckYAMLTree(data); err != nil {
		return nil, effectiveDoc{}, wrapYAML(err)
	}

	var doc sourceDoc
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return nil, effectiveDoc{}, wrapYAML(err)
	}

	cfg, err := normalizeSource(doc)
	if err != nil {
		return nil, effectiveDoc{}, err
	}

	applyDefaults(cfg)
	if err := validate(cfg); err != nil {
		return nil, effectiveDoc{}, err
	}
	return cfg, effectiveSource(doc, cfg), nil
}

// checkShape ensures the document is a single mapping with a single copy
// of each top-level key. Multi-document YAML is rejected outright (fail
// closed): the strict decode below reads only the first document, so any
// content after a `---` marker would otherwise be silently discarded —
// e.g. an appended override file or a `security:` block — while config
// check reports valid.
func checkShape(data []byte) error {
	if err := CheckSingleDocument(data); err != nil {
		return err
	}
	var m map[string]yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&m); err != nil {
		return err
	}
	return nil
}

// CheckSingleDocument ensures data is exactly one YAML document: not
// empty and with no second document after a `---` marker. It is shared by
// the config loader and the auth users-file loader so a trailing document
// is never silently discarded (FIX-14, PLAN §28); the strict decoders only
// ever read the first document, so anything after the marker would
// otherwise be lost while validation reports success.
func CheckSingleDocument(data []byte) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var first any
	if err := dec.Decode(&first); err != nil {
		if err == io.EOF {
			return fmt.Errorf("empty YAML document")
		}
		return err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multi-document YAML is not allowed")
		}
		return err
	}
	return nil
}

// CheckYAMLTree walks the full YAML tree and rejects aliases/anchors and
// non-standard tags (PLAN §28). It is the shared strictness check used by
// both the config loader and the auth users-file loader, so a wildcard
// ACL hidden inside a `<<:` merge key cannot evade review (T-M9).
func CheckYAMLTree(data []byte) error {
	var root yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&root); err != nil {
		return err
	}
	var walk func(n *yaml.Node) error
	walk = func(n *yaml.Node) error {
		if n.Kind == yaml.AliasNode {
			return fmt.Errorf("line %d: YAML aliases and anchors are not allowed", n.Line)
		}
		if !allowedTag(n.Tag) {
			return fmt.Errorf("line %d: disallowed YAML tag %q", n.Line, n.Tag)
		}
		for _, c := range n.Content {
			if err := walk(c); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(&root)
}

// allowedTag reports whether a YAML node tag is one of the plain
// structural/scalar tags Mellomting configuration may use.
func allowedTag(tag string) bool {
	switch {
	case tag == "":
		return true
	case strings.HasPrefix(tag, "!!"):
		tag = strings.TrimPrefix(tag, "!!")
	case strings.HasPrefix(tag, "tag:yaml.org,2002:"):
		tag = strings.TrimPrefix(tag, "tag:yaml.org,2002:")
	default:
		// Custom tags (!foo) or foreign namespaces (tag:other:x).
		return false
	}
	switch tag {
	case "", "str", "int", "float", "bool", "null", "seq", "map", "timestamp":
		return true
	}
	return false
}

// wrapYAML gives YAML errors a stable prefix and trims the noisy
// "while parsing..." prefix when line information is already present.
func wrapYAML(err error) error {
	return fmt.Errorf("invalid YAML configuration: %v", err)
}
