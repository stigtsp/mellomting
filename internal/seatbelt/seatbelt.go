// Package seatbelt implements the post-startup containment on macOS
// (PLAN §53-63), the Seatbelt counterpart of internal/landlock.
//
// The policy is translated into a Sandbox Profile Language profile that
// denies everything by default, imports Apple's own baseline for BSD
// daemons, and grants back exactly the capabilities the daemon still
// needs. A profile is compiled before it is applied, so a profile the
// system cannot accept is a startup error rather than a half-confined
// process.
//
// Seatbelt is macOS-only. On other platforms Check reports it as
// unavailable and Apply fails; Mellomting never silently degrades a
// required sandbox to none (PLAN §7, §55).
package seatbelt

import (
	"fmt"
	"path/filepath"
	"strings"

	"mellomting/internal/sandbox"
)

// Backend is the name this enforcement mechanism reports itself under.
const Backend = "seatbelt"

// baseProfile is Apple's own profile for "various BSD daemons". It
// imports system.sb and grants the reads a running process needs —
// shared libraries, timezone data, locale tables — none of which the
// daemon's own policy should have to enumerate, and all of which Apple
// keeps current across releases. Neither file sets a default action, so
// the deny below governs everything they do not name.
const baseProfile = "bsd.sb"

// Profile renders pol as a Sandbox Profile Language profile.
//
// The order is significant: SBPL takes the last matching rule, so the
// blanket deny comes first and every grant after it. Paths are embedded
// as SBPL string literals rather than passed as profile parameters,
// which keeps the compiled profile self-contained and reviewable in a
// log.
func Profile(pol sandbox.Policy) string {
	var b strings.Builder
	b.WriteString("(version 1)\n(deny default)\n(import \"" + baseProfile + "\")\n")

	// The users file is re-read by pathname on SIGHUP (PLAN §30), and
	// every key mutation publishes it by renaming a new file over the
	// old one, so the grant covers the directory rather than an inode
	// that stops existing at the first `key create` (PLAN §58).
	for _, p := range pol.ReadPaths {
		for _, path := range pathForms(p) {
			b.WriteString("(allow file-read* (subpath " + quote(path) + "))\n")
		}
	}
	// The accounting log is opened once and appended to; the write
	// grant is per-file, never per-directory.
	for _, p := range pol.WriteFiles {
		for _, path := range pathForms(p) {
			b.WriteString("(allow file-write-data file-write-flags (literal " + quote(path) + "))\n")
		}
	}

	// The listener is already bound and listening, but Seatbelt filters
	// accepts as well as binds, so a policy that named neither would
	// confine the daemon into answering nothing.
	switch {
	case pol.Listen.UnixPath != "":
		for _, path := range pathForms(pol.Listen.UnixPath) {
			b.WriteString("(allow network-inbound (local unix-socket (path-literal " + quote(path) + ")))\n")
			// Closing the listener unlinks the socket (PLAN §74).
			b.WriteString("(allow file-write-unlink (literal " + quote(path) + "))\n")
		}
	case pol.Listen.TCPPort != 0:
		b.WriteString(fmt.Sprintf("(allow network-inbound (local ip \"*:%d\"))\n", pol.Listen.TCPPort))
	}

	// Backend connections are filtered by destination port only. That
	// is not a shortcut: SBPL accepts just "*" or "localhost" as the
	// host of a network address, so the destination cannot be narrowed
	// further here — restricting the address is the backend network
	// mode's job either way (PLAN §16, §60), exactly as under Landlock.
	for _, port := range pol.ConnectTCP {
		b.WriteString(fmt.Sprintf("(allow network-outbound (remote tcp \"*:%d\"))\n", port))
	}
	return b.String()
}

// pathForms returns the pathnames a rule must name to match: the one
// configured, and the one symlinks resolve it to when they differ.
//
// Seatbelt matches the literal path the kernel evaluates, not the one
// the operator wrote, and the macOS root is a field of symlinks that
// make those differ: /etc, /var and /tmp are all links into /private.
// The default configuration puts the users file under /etc/mellomting
// and the accounting log under /var/log/mellomting, so a policy naming
// only what the operator wrote would leave the daemon unable to reload
// its own key store or record usage — after compiling and applying
// without complaint. Both forms are granted, because the traversal
// itself begins at the configured name.
//
// A path that cannot be resolved — it does not exist yet, or something
// above it is unreadable — is granted as written: that is the pathname
// the daemon will use.
func pathForms(path string) []string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved == path {
		return []string{path}
	}
	return []string{path, resolved}
}

// quote renders s as an SBPL string literal. SBPL strings are Scheme
// strings, so a backslash or a double quote in a pathname has to be
// escaped or it would end the literal early and change which paths the
// profile grants.
func quote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := range len(s) {
		if c := s[i]; c == '\\' || c == '"' {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('"')
	return b.String()
}
