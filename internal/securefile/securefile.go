// Package securefile reads sensitive local files defensively (PLAN §28):
// the final path component is opened without following a symlink, the
// target must be a regular file, and the returned content is size-bounded.
package securefile

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// DefaultMaxSize bounds sensitive files when no explicit bound is passed.
const DefaultMaxSize = 1 << 20

// Read opens path without following the final component if it is a
// symlink, verifies the target is a regular file, rejects a mode that is
// accessible by others (T-M8), and returns at most maxBytes of content
// (DefaultMaxSize when maxBytes <= 0). Sensitive files are secret-bearing
// (users, pepper, TLS key, backend api_key_file), so a group- or
// world-readable mode is refused fail-closed rather than read.
func Read(path string, maxBytes int64) ([]byte, error) {
	return read(path, maxBytes, true)
}

// ReadPublic is Read without the mode check: the target must still be a
// regular file reached through no final symlink (O_NOFOLLOW, PLAN §28) and
// is size-bounded, but a world-readable mode is accepted. It is for public
// data only — the TLS certificate, which is 0644 in mainstream issuance
// (certbot fullchain.pem) and carries no secret (FIX-06/M31). Private keys
// must keep going through Read.
func ReadPublic(path string, maxBytes int64) ([]byte, error) {
	return read(path, maxBytes, false)
}

func read(path string, maxBytes int64, rejectWorldAccessible bool) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxSize
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	if rejectWorldAccessible && WorldAccessible(info.Mode()) {
		return nil, fmt.Errorf("mode %04o is too permissive: file must not be readable by others (use 0600 or 0640)", info.Mode().Perm())
	}

	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("exceeds maximum size of %d bytes", maxBytes)
	}
	return data, nil
}

// WorldAccessible reports whether the (non-symlink) file has any
// permission bit set for "others" (read, write, or execute). The exposure
// is readability: a 0644/0666 secret file is rejected while 0600/0640 are
// accepted (PLAN §100, T-M8).
func WorldAccessible(mode os.FileMode) bool {
	return mode.Perm()&0o007 != 0
}
