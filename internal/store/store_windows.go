//go:build windows

package store

import (
	"fmt"
	"os"
	"os/exec"
)

const noFollowFlag = 0

func openNoFollowLockFile(lockPath string) (*os.File, error) {
	if fi, err := os.Lstat(lockPath); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("store: refusing symlink lock file %s", lockPath)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	st, sErr := f.Stat()
	if sErr != nil {
		_ = f.Close()
		return nil, sErr
	}
	if !st.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("store: refusing non-regular lock file %s", lockPath)
	}
	return f, nil
}

// OpenReadNoFollow opens an existing regular file read-only, verifying regular file status before and after open.
func OpenReadNoFollow(path string) (*os.File, error) {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("store: refusing symlink file %s", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("store: refusing non-regular file %s", path)
	}
	return f, nil
}

// VerifyOwnedPrivateFile verifies that fi is a regular non-symlink file on Windows.
func VerifyOwnedPrivateFile(fi os.FileInfo) bool {
	return fi != nil && fi.Mode()&os.ModeSymlink == 0 && fi.Mode().IsRegular()
}

// SetDetachedSession is a portable stub on Windows.
func SetDetachedSession(_ *exec.Cmd) {}

// SetProcessGroup is a portable stub on Windows.
func SetProcessGroup(_ *exec.Cmd) {}

// KillProcessGroup is a portable stub on Windows (cmd.Process.Kill terminates the process).
func KillProcessGroup(_ int) {}

func flockExclusive(_ *os.File, _ bool) error {
	return nil
}

func flockUnlock(_ *os.File) error {
	return nil
}
