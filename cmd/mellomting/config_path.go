package main

import (
	"mellomting/internal/config"
)

// ResolveConfigPath implements the D1 configuration lookup used by every
// config-dependent CLI command: an explicit --config PATH when supplied
// (never falling back, even when the named path does not exist), and
// otherwise /etc/mellomting/config.yaml.
//
// The working directory is never consulted: these commands run as root, so
// an implicit ./config.yaml would let whoever can write the directory pick
// the auth files, backends, and sandbox mode. Name it to use it.
func ResolveConfigPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	return config.DefaultConfigPath
}
