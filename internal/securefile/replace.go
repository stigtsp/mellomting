package securefile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Replace atomically replaces path with the bytes write emits, applying
// every guarantee a privileged write needs:
//
//   - a symlink or non-regular destination is refused, so a replace
//     cannot be redirected by whoever can create a name in the directory;
//   - the content is written to a temporary file in the destination
//     directory and fsynced before the rename, because rename(2) orders
//     the directory entry and not the file's blocks — an unsynced write
//     can leave a truncated file after a crash;
//   - the mode is set through the file descriptor, never by path, so it
//     cannot land on a file swapped in underneath;
//   - the parent directory is fsynced after the rename, so the new name
//     itself survives a crash; and
//   - the temporary file is removed on every failure path.
//
// These were previously re-derived at four call sites, each missing a
// different member of the set.
func Replace(path string, mode os.FileMode, write func(io.Writer) error) error {
	return replace(path, mode, 0, write)
}

// ReplacePreservingOwner is Replace, except that the result stays
// readable by the service account:
//
//   - when path already exists the replacement inherits its owner and
//     takes its mode clamped by clamp (never widened);
//   - when path does not exist it adopts the group of the directory it
//     is created in, if that group is not the caller's own, and mode
//     gains group-read within clamp.
//
// The second rule is what keeps `key create` from writing a 0600
// root:root users file the daemon can never read. The installer sets
// the configuration directory to root:<service group> precisely so the
// files inside it are group-readable, and a file created later belongs
// to the same arrangement as one created by the installer itself
// (PLAN §29.1).
func ReplacePreservingOwner(path string, mode, clamp os.FileMode, write func(io.Writer) error) error {
	return replace(path, mode, clamp, write)
}

// ServiceGroup reports the group a file created in a directory should
// take: the directory's own group when it differs from the caller's
// effective group, the signal that the directory was set up for a
// distinct service account (root:mellomting by the installer and the
// Debian package) rather than merely inheriting the caller's. Such a
// file also gains group-read (PLAN §29.1). The chown is best-effort:
// an unprivileged caller outside that group keeps the file in its own
// group at the narrower mode, as ReplacePreservingOwner does. init
// applies the same rule to the files it publishes.
func ServiceGroup(dirGid, egid int) (gid int, ok bool) {
	if dirGid == egid {
		return 0, false
	}
	return dirGid, true
}

// dirGroup applies ServiceGroup to the directory at dir.
func dirGroup(dir string) (int, bool) {
	st, err := os.Stat(dir)
	if err != nil {
		return 0, false
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return ServiceGroup(int(sys.Gid), os.Getegid())
}

func replace(path string, mode, clamp os.FileMode, write func(io.Writer) error) error {
	existing, statErr := os.Lstat(path)
	switch {
	case statErr == nil:
		if existing.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to replace %q: it is a symlink", path)
		}
		if !existing.Mode().IsRegular() {
			return fmt.Errorf("refusing to replace %q: it is not a regular file", path)
		}
	case !errors.Is(statErr, fs.ErrNotExist):
		return fmt.Errorf("stat %q: %w", path, statErr)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".mellomting-"+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	closed := false
	abort := func() {
		if !closed {
			_ = tmp.Close()
		}
		_ = os.Remove(tmpName)
	}

	switch {
	case clamp == 0:
	case statErr == nil:
		mode = existing.Mode().Perm() & clamp
		if st, ok := existing.Sys().(*syscall.Stat_t); ok {
			// Through the descriptor, not the path, for the same reason
			// the mode is: a path-based chown follows a symlink and can
			// land on a file swapped in underneath.
			//
			// Best-effort: an unprivileged caller cannot chown to a group
			// it is not in. The mode clamp still applies and the rename
			// below proceeds regardless.
			_ = tmp.Chown(int(st.Uid), int(st.Gid))
		}
	default:
		// A file that does not exist yet has no owner to preserve, so it
		// takes the directory's service group instead. The mode widens
		// only once the chown has actually succeeded: a group-readable
		// file still owned by the caller's own group is wider than the
		// 0600 it would otherwise get.
		if gid, ok := dirGroup(dir); ok && tmp.Chown(-1, gid) == nil {
			mode = (mode | 0o040) & clamp
		}
	}

	if werr := write(tmp); werr != nil {
		abort()
		return werr
	}
	if err := tmp.Sync(); err != nil {
		abort()
		return fmt.Errorf("sync %q: %w", tmpName, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		abort()
		return fmt.Errorf("chmod %q: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		closed = true
		abort()
		return fmt.Errorf("close %q: %w", tmpName, err)
	}
	closed = true
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace %q: %w", path, err)
	}
	// Fsync the parent so the rename itself is durable. Best-effort: a
	// directory that cannot be opened does not invalidate the write.
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
