//go:build !windows

package store

import (
	"os"
	"syscall"
)

const noFollowFlag = syscall.O_NOFOLLOW

func openNoFollowLockFile(lockPath string) (*os.File, error) {
	return os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
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
