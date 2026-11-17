package agent

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"time"
)

// CheckEdge asks the edge whether it would accept the agent key, without
// attaching: a GET on the attach path with no upgrade is answered 426 for
// a valid agent key once agents are enabled, 401 for any other key, and
// 404 when agents are disabled. Nothing is registered.
func CheckEdge(ctx context.Context, o Options) error {
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	u := *o.Edge
	u.Path = path.Join("/", o.Edge.Path, AttachPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+o.Key)
	if o.UserAgent != "" {
		req.Header.Set("User-Agent", o.UserAgent)
	}
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:           nil,
			DialContext:     newDialer(handshakeTimeout).DialContext,
			TLSClientConfig: &tls.Config{RootCAs: o.RootCAs, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}},
		},
		// A redirect could carry the key to another scheme or host,
		// past the attach.insecure_edge check; the attach never follows one.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach the edge: %w", err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUpgradeRequired:
		return nil
	case http.StatusUnauthorized:
		return errors.New("the edge rejected the agent key (unknown, disabled, expired, or not an agent key)")
	case http.StatusNotFound:
		return errors.New("agents are not enabled on the edge (agents.enabled)")
	default:
		return fmt.Errorf("unexpected answer from the edge: %s", resp.Status)
	}
}

// ProbeBackend lists the models the local server advertises.
func ProbeBackend(ctx context.Context, o Options) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ms, err := probeBackend(ctx, o)
	if err != nil {
		return nil, err
	}
	return modelIDs(ms), nil
}
