package main

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"mellomting/internal/agent"
	"mellomting/internal/backend"
	"mellomting/internal/config"
	"mellomting/internal/securefile"
	"mellomting/internal/version"
)

// attachOptions reads what the attach section names (the keys, the edge
// CA and the system trust store) before the sandbox, and returns one
// agent.Options per server (PLAN §104). The trust store is preloaded for
// the reason buildDaemon preloads it: crypto/x509 would otherwise read it
// on the first handshake, after confinement.
func attachOptions(a *config.Attach, log *slog.Logger) ([]agent.Options, error) {
	key, err := readSecret(a.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("attach.key_file: %w", err)
	}
	return attachOptionsWithKey(a, key, log)
}

// attachOptionsWithKey is attachOptions with the agent key in hand.
func attachOptionsWithKey(a *config.Attach, key string, log *slog.Logger) ([]agent.Options, error) {
	edge, err := config.ParseEdgeURL(a.Edge)
	if err != nil {
		return nil, fmt.Errorf("attach.edge: %w", err)
	}
	var backendKey string
	if a.BackendKeyFile != "" {
		if backendKey, err = readSecret(a.BackendKeyFile); err != nil {
			return nil, fmt.Errorf("attach.backend_key_file: %w", err)
		}
	}
	system, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("load system trust store: %w", err)
	}
	roots := system
	if a.CAFile != "" {
		if roots, err = readCAFile(a.CAFile); err != nil {
			return nil, fmt.Errorf("attach.ca_file: %w", err)
		}
	}
	opts := make([]agent.Options, 0, len(a.Servers))
	for _, s := range a.Servers {
		u, err := backend.ParseBaseURL(s)
		if err != nil {
			return nil, fmt.Errorf("attach.servers: %w", err)
		}
		opts = append(opts, agent.Options{
			Edge:           edge,
			Key:            key,
			Backend:        u,
			BackendKey:     backendKey,
			RootCAs:        roots,
			BackendRootCAs: system,
			UserAgent:      "mellomting-agent/" + version.String(),
			Log:            log.With("component", "agent", "backend", u.Redacted()),
		})
	}
	return opts, nil
}

func readCAFile(path string) (*x509.CertPool, error) {
	pem, err := securefile.ReadPublic(path, 1<<20)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("no certificates found")
	}
	return pool, nil
}

// readSecret reads a one-line secret from a file that only its owner, and
// at most its group, can read.
func readSecret(path string) (string, error) {
	data, err := securefile.Read(path, 4096)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return "", errors.New("file is empty")
	}
	return s, nil
}

// attachments runs one agent per server until stopped.
type attachments struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func startAttachments(opts []agent.Options, log *slog.Logger) *attachments {
	ctx, cancel := context.WithCancel(context.Background())
	a := &attachments{cancel: cancel}
	for _, o := range opts {
		a.wg.Go(func() { agent.Run(ctx, o) })
	}
	log.Info("attaching", "edge", opts[0].Edge.Redacted(), "servers", len(opts))
	return a
}

// stop begins draining every attachment and returns a channel closed
// once all have detached. The caller bounds the wait: each attachment
// lets in-flight streams finish, which the shutdown grace period cuts
// short by exiting.
func (a *attachments) stop() <-chan struct{} {
	a.cancel()
	done := make(chan struct{})
	go func() { a.wg.Wait(); close(done) }()
	return done
}
