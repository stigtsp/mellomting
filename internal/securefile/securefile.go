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
	if WorldAccessible(info.Mode()) {
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

// LStat reports the mode of the final path component without following a
// symlink. A symlink is an error, not a mode to report.
func LStat(path string) (os.FileMode, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return 0, fmt.Errorf("symlink not allowed")
	}
	return info.Mode(), nil
}

// WorldAccessible reports whether the (non-symlink) file has any
// permission bit set for "others" (read, write, or execute). The exposure
// is readability: a 0644/0666 secret file is rejected while 0600/0640 are
// accepted (PLAN §100, T-M8).
func WorldAccessible(mode os.FileMode) bool {
	return mode.Perm()&0o007 != 0
}
