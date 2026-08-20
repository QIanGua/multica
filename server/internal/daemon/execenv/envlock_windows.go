//go:build windows

package execenv

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockFileExclusiveNonBlocking takes an exclusive lock on f without waiting.
// ok is false when another process already holds it.
//
// Windows releases the lock when the handle closes, including on abnormal
// process termination, giving the same crash-safe liveness answer as flock on
// unix — see the unix build of this file for why that property matters.
func lockFileExclusiveNonBlocking(f *os.File) (ok bool, err error) {
	overlapped := new(windows.Overlapped)
	err = windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, overlapped,
	)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return false, nil
	}
	return false, err
}

// unlockFile drops the lock.
func unlockFile(f *os.File) error {
	overlapped := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, overlapped)
}
