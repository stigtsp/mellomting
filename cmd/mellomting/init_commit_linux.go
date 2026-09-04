//go:build linux

package main

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// productionCommitOps implements the D4 directory-FD-relative, create-only
// publication contract using narrow x/sys primitives.
type productionCommitOps struct{}

func defaultCommitOps() commitOps { return productionCommitOps{} }

func commitIdentity(st *unix.Stat_t) commitFileIdentity {
	return commitFileIdentity{
		Dev:  uint64(st.Dev),
		Ino:  uint64(st.Ino),
		Mode: uint64(st.Mode),
		Uid:  uint64(st.Uid),
	}
}

func (productionCommitOps) openParent(path string) (int, commitFileIdentity, error) {
	fd, err := unix.Openat(unix.AT_FDCWD, path, unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, commitFileIdentity{}, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return 0, commitFileIdentity{}, err
	}
	return fd, commitIdentity(&st), nil
}

func (productionCommitOps) fstatParent(fd int) (commitFileIdentity, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return commitFileIdentity{}, err
	}
	return commitIdentity(&st), nil
}

func (productionCommitOps) lstatInDir(fd int, name string) (commitFileIdentity, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return commitFileIdentity{}, err
	}
	return commitIdentity(&st), nil
}

func (productionCommitOps) createInDir(fd int, name string, mode uint32) (int, commitFileIdentity, error) {
	f, err := unix.Openat(fd, name, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC, mode)
	if err != nil {
		return 0, commitFileIdentity{}, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(f, &st); err != nil {
		_ = unix.Close(f)
		return 0, commitFileIdentity{}, err
	}
	return f, commitIdentity(&st), nil
}

func (productionCommitOps) writeAll(fd int, name string, data []byte) error {
	for len(data) > 0 {
		n, err := unix.Write(fd, data)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return fmt.Errorf("write %s: %w", name, err)
		}
		data = data[n:]
	}
	return nil
}

func (productionCommitOps) fsyncFile(fd int, name string) error {
	if err := unix.Fsync(fd); err != nil {
		return fmt.Errorf("fsync %s: %w", name, err)
	}
	return nil
}

func (productionCommitOps) fstatFile(fd int, name string) (commitFileIdentity, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return commitFileIdentity{}, err
	}
	return commitIdentity(&st), nil
}

func (productionCommitOps) closeFile(fd int, name string) error {
	if err := unix.Close(fd); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	return nil
}

func (productionCommitOps) linkInDir(fd int, oldname, newname string) error {
	if err := unix.Linkat(fd, oldname, fd, newname, 0); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("%s appeared before publication", newname)
		}
		return err
	}
	return nil
}

func (productionCommitOps) unlinkInDir(fd int, name string) error {
	if err := unix.Unlinkat(fd, name, 0); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	return nil
}

func (productionCommitOps) fsyncDir(fd int) error {
	if err := unix.Fsync(fd); err != nil {
		return fmt.Errorf("fsync directory: %w", err)
	}
	return nil
}
