package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"mellomting/internal/config"
)

// ResolveConfigPath implements the D1 configuration lookup used by every
// config-dependent CLI command. It returns the path to load, in priority
// order:
//
//  1. an explicit --config PATH, when supplied (never falls back, even when
//     the named path does not exist);
//  2. ./config.yaml, when it exists as a regular non-symlink file;
//  3. /etc/mellomting/config.yaml, only when ./config.yaml is absent.
//
// If the local path exists as a symlink (including dangling), directory,
// FIFO, device, or other non-regular file, resolution fails instead of
// falling through to /etc.
func ResolveConfigPath(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	st, err := os.Lstat("./config.yaml")
	switch {
	case err == nil:
		if !st.Mode().IsRegular() {
			return "", fmt.Errorf("./config.yaml exists but is not a regular file; remove or replace it, or pass --config explicitly")
		}
		return "./config.yaml", nil
	case errors.Is(err, fs.ErrNotExist):
		return config.DefaultConfigPath, nil
	default:
		return "", fmt.Errorf("checking ./config.yaml: %w", err)
	}
}
