//go:build !linux

package main

import "errors"

// defaultCommitOps is a fail-closed stub on non-Linux hosts: the D4
// directory-FD/linkat contract is implemented only with the reviewed Linux
// x/sys primitives.
type unsupportedCommitOps struct{}

func defaultCommitOps() commitOps { return unsupportedCommitOps{} }

func (unsupportedCommitOps) openParent(string) (int, commitFileIdentity, error) {
	return 0, commitFileIdentity{}, errors.New("local init commit is Linux-only")
}

func (unsupportedCommitOps) fstatParent(int) (commitFileIdentity, error) {
	return commitFileIdentity{}, errors.New("local init commit is Linux-only")
}

func (unsupportedCommitOps) lstatInDir(int, string) (commitFileIdentity, error) {
	return commitFileIdentity{}, errors.New("local init commit is Linux-only")
}

func (unsupportedCommitOps) createInDir(int, string, uint32) (int, commitFileIdentity, error) {
	return 0, commitFileIdentity{}, errors.New("local init commit is Linux-only")
}

func (unsupportedCommitOps) writeAll(int, string, []byte) error {
	return errors.New("local init commit is Linux-only")
}

func (unsupportedCommitOps) fsyncFile(int, string) error {
	return errors.New("local init commit is Linux-only")
}

func (unsupportedCommitOps) fstatFile(int, string) (commitFileIdentity, error) {
	return commitFileIdentity{}, errors.New("local init commit is Linux-only")
}

func (unsupportedCommitOps) closeFile(int, string) error {
	return errors.New("local init commit is Linux-only")
}

func (unsupportedCommitOps) linkInDir(int, string, string) error {
	return errors.New("local init commit is Linux-only")
}

func (unsupportedCommitOps) unlinkInDir(int, string) error {
	return errors.New("local init commit is Linux-only")
}

func (unsupportedCommitOps) fsyncDir(int) error {
	return errors.New("local init commit is Linux-only")
}
