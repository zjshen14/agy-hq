//go:build !windows

package store

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

const noFollowFlag = syscall.O_NOFOLLOW

func openNoFollowLockFile(lockPath string) (*os.File, error) {
	return os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
}

// OpenReadNoFollow opens an existing regular file read-only without following terminal symlinks.
func OpenReadNoFollow(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
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

// VerifyOwnedPrivateFile verifies that fi is a regular file owned by the current UID with no group/other permissions.
func VerifyOwnedPrivateFile(fi os.FileInfo) bool {
	if fi == nil || fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return false
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return false
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		if st.Uid != uint32(os.Getuid()) {
			return false
		}
	}
	return true
}

// SetDetachedSession configures cmd to run in a new POSIX session (`Setsid: true`).
func SetDetachedSession(cmd *exec.Cmd) {
	if cmd != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
}

// SetProcessGroup configures cmd to run in its own POSIX process group (`Setpgid: true`).
func SetProcessGroup(cmd *exec.Cmd) {
	if cmd != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
}

// KillProcessGroup sends SIGKILL to the entire POSIX process group `-pid`.
func KillProcessGroup(pid int) {
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}

func flockExclusive(f *os.File, nonBlocking bool) error {
	how := syscall.LOCK_EX
	if nonBlocking {
		how |= syscall.LOCK_NB
	}
	return syscall.Flock(int(f.Fd()), how)
}

func flockUnlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
