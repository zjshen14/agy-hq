//go:build windows

package store

import "os"

const noFollowFlag = 0

func openNoFollowLockFile(lockPath string) (*os.File, error) {
	return os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
}

func flockExclusive(_ *os.File, _ bool) error {
	return nil
}

func flockUnlock(_ *os.File) error {
	return nil
}
