package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"mellomting/internal/auth"
	"mellomting/internal/config"
	"mellomting/internal/logging"
	"mellomting/internal/securefile"
	"mellomting/internal/systemd"
)

// File names `init --edge` writes beside the configuration.
const (
	attachKeyName        = "agent.key"
	attachBackendKeyName = "backend.key"
	attachCAName         = "edge-ca.pem"
)

// initAttach are the inputs of `mellomting init --edge`.
type initAttach struct {
	configPath     string
	edge           string
	servers        []string
	caFile         string
	backendKeyFile string
	insecureEdge   bool
	sandbox        initSandbox
	sandboxMode    string
	dryRun         bool
	unitInstalled  bool
}

// initAttachCmd checks the flags that make no sense with --edge and the
// sandbox, then runs runInitAttach.
func initAttachCmd(a initAttach, listenSet, sandboxSet bool) int {
	if listenSet {
		fmt.Fprintln(os.Stderr, "mellomting: init: --listen and --edge exclude each other: a daemon that attaches serves no clients")
		return 2
	}
	if len(a.servers) == 0 {
		a.servers = []string{defaultAttachServer}
	}
	for _, s := range a.servers {
		if name, _, ok := strings.Cut(s, "="); ok && initServerNameLike.MatchString(name) {
			fmt.Fprintf(os.Stderr, "mellomting: init: --server %q: with --edge servers are not named; give the URL alone\n", s)
			return 2
		}
	}
	if u, err := config.ParseEdgeURL(a.edge); err == nil && u.Scheme == "http" &&
		config.IsNonLoopbackHost(u.Hostname()) && !a.insecureEdge {
		fmt.Fprintln(os.Stderr, "mellomting: init: --edge must be https unless it is a loopback address (the agent key would travel unencrypted); use --insecure-edge to override")
		return 2
	}
	if err := a.sandbox.preflight(a.sandboxMode, sandboxSet, initSandboxCheck); err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: init: %v\n", err)
		return 1
	}
	if !a.dryRun && !initCommitSupported {
		fmt.Fprintln(os.Stderr, "mellomting: init: creating files requires Linux; use --dry-run to preview the configuration")
		return 1
	}
	return runInitAttach(a, os.Stdin)
}

// defaultAttachServer is the vLLM default address.
const defaultAttachServer = "http://127.0.0.1:8000"

// runInitAttach writes a configuration that only attaches (PLAN §104):
// the agent key from standard input, copies of the CA and backend key,
// and the attach section naming them. The key is checked with the edge
// first, and nothing is written if it is refused. The copies sit beside
// the configuration so the service account can read them wherever the
// originals were.
func runInitAttach(a initAttach, stdin io.Reader) int {
	abs, err := filepath.Abs(a.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: init: --config: %v\n", err)
		return 2
	}
	dir := filepath.Dir(abs)
	att := &config.Attach{
		Edge:         a.edge,
		KeyFile:      filepath.Join(dir, attachKeyName),
		Servers:      a.servers,
		InsecureEdge: a.insecureEdge,
	}
	var ca, backendKey []byte
	if a.caFile != "" {
		if ca, err = securefile.ReadPublic(a.caFile, 1<<20); err != nil {
			fmt.Fprintf(os.Stderr, "mellomting: init: --ca-file: %v\n", err)
			return 1
		}
		att.CAFile = filepath.Join(dir, attachCAName)
	}
	if a.backendKeyFile != "" {
		k, err := readSecret(a.backendKeyFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mellomting: init: --backend-key-file: %v\n", err)
			return 1
		}
		backendKey = []byte(k + "\n")
		att.BackendKeyFile = filepath.Join(dir, attachBackendKeyName)
	}

	doc := map[string]any{
		"version":  1,
		"security": map[string]any{a.sandbox.section: a.sandbox.doc(a.sandboxMode)},
		"attach":   att,
	}
	rendered, err := yaml.Marshal(doc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: init: render configuration: %v\n", err)
		return 1
	}
	if _, err := config.Parse(rendered); err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: init: %v\n", err)
		return 2
	}
	if a.dryRun {
		_, _ = os.Stdout.Write(rendered)
		return 0
	}

	key, err := readKeyArg(stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: init: agent key: %v\n", err)
		return 2
	}
	// Check against what serve will use, with the files not yet written:
	// the originals stand in for the copies.
	probe := *att
	probe.CAFile, probe.BackendKeyFile = a.caFile, a.backendKeyFile
	log, _ := logging.New(io.Discard, "error", "text")
	opts, err := attachOptionsWithKey(&probe, key, log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: init: %v\n", err)
		return 1
	}
	if code := checkAttach(opts, "init"); code != 0 {
		fmt.Fprintln(os.Stderr, "mellomting: init: nothing written")
		return code
	}

	files := []commitFile{{path: att.KeyFile, data: []byte(key + "\n")}}
	if backendKey != nil {
		files = append(files, commitFile{path: att.BackendKeyFile, data: backendKey})
	}
	if ca != nil {
		files = append(files, commitFile{path: att.CAFile, data: ca})
	}
	files = append(files, commitFile{path: abs, data: rendered})
	restore, err := setScaffoldAside(abs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: init: %v\n", err)
		return 1
	}
	if err := commitFiles(files, nil); err != nil {
		restore(false)
		fmt.Fprintf(os.Stderr, "mellomting: init: %v\n", err)
		return 1
	}
	restore(true)
	configFlag := " --config " + abs
	if abs == config.DefaultConfigPath {
		configFlag = ""
	}
	if a.unitInstalled {
		fmt.Fprintf(os.Stdout, "initialized %s\nnext:\n  sudo systemctl enable --now %s\n", abs, systemd.UnitName)
	} else {
		fmt.Fprintf(os.Stdout, "initialized %s\nnext:\n  mellomting serve%s\n", abs, configFlag)
	}
	return 0
}

// setScaffoldAside moves the placeholder `install --systemd` writes out
// of init's way, so init can take its place; a configuration anyone has
// edited stays for commitFiles to refuse. restore(false) puts the
// placeholder back after a failed commit, restore(true) discards it.
func setScaffoldAside(path string) (restore func(committed bool), err error) {
	restore = func(bool) {}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, systemd.ConfigTemplate()) {
		return restore, nil
	}
	aside := path + ".install-scaffold"
	if err := os.Rename(path, aside); err != nil {
		return restore, err
	}
	return func(committed bool) {
		if committed {
			_ = os.Remove(aside)
		} else {
			_ = os.Rename(aside, path)
		}
	}, nil
}

// readKeyArg reads the agent key from standard input and checks that it
// is shaped like a Mellomting key.
func readKeyArg(r io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(r, 4096))
	if err != nil {
		return "", err
	}
	key := strings.TrimSpace(string(data))
	if key == "" {
		return "", errors.New("empty (pipe it on standard input)")
	}
	if _, err := auth.Parse(key); err != nil {
		return "", errors.New("not a Mellomting key")
	}
	return key, nil
}
