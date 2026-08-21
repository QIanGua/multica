package execenv

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TaskTempDirPrefix is the basename prefix os.MkdirTemp stamps on every
// per-task temp directory the daemon creates under the task temp base. The
// sweep below removes nothing without it, so the base itself — usually a
// shared /tmp holding other programs' files — is never at risk.
const TaskTempDirPrefix = "multica-task-"

// LockTaskTempDir takes the temp directory's execution lock and returns the
// held file; the caller owns it for the lifetime of the task run.
//
// This is the same .task_lock an env root uses, for the same reason: the
// kernel releases it when the holding process dies, so it answers "is the
// execution that owns this directory still alive?" across processes without a
// heartbeat, a PID table, or a stale-state cleanup path. That is what lets
// PruneTaskTempDirs reclaim a directory the moment its owner is gone while
// never touching one that is still in use — including a directory owned by a
// different daemon sharing the same temp base, which no in-memory active set
// could ever see, and including a live task in this very process, because a
// lock is held by an open file description rather than by a process.
func LockTaskTempDir(dir string) (*os.File, error) {
	lock, err := openLockFile(filepath.Join(dir, envRootLockFile))
	if err != nil {
		return nil, fmt.Errorf("open task temp dir lock for %s: %w", dir, err)
	}
	locked, err := lockFileExclusiveNonBlocking(lock)
	if err != nil {
		lock.Close()
		return nil, fmt.Errorf("lock task temp dir %s: %w", dir, err)
	}
	if !locked {
		// Unreachable in practice: the directory was just created by
		// os.MkdirTemp under a name nobody else knows yet.
		lock.Close()
		return nil, fmt.Errorf("task temp dir %s is already locked", dir)
	}
	return lock, nil
}

// ReleaseTaskTempLock drops the temp directory's execution lock and closes the
// file. Safe on nil and safe to call twice.
func ReleaseTaskTempLock(f *os.File) {
	releaseLockFile(f)
}

// PruneTaskTempDirs reclaims per-task temp directories whose owning execution
// is gone, under the base returned by the daemon's task temp base resolution.
//
// These directories are the agent process's TMPDIR. They live outside
// WorkspacesRoot — the task GC's scan root — so nothing else ever reclaims
// them: their only other exit is the RemoveAll the daemon defers at the end of
// runTask, which does not run when the daemon is killed and does not succeed
// when a file inside is still open (the Windows case in #7364). Whatever that
// call misses accumulates on disk forever.
//
// Liveness comes from the lock, not from the clock. A directory is removed
// only once this sweep has itself acquired the .task_lock its owner would
// still be holding, which the kernel grants only after that owner is gone.
//
// The lock is held by the daemon, not by the agent process, so it answers "is
// the owning EXECUTION still alive?" and not "is every child it spawned gone?".
// A daemon killed while agent processes survive it releases the lock on the
// spot, and this sweep may then reclaim a directory such an orphan is still
// writing to. That is the same exposure an env root's .task_lock already
// carries, and from the platform's point of view the task is dead either way;
// closing it properly is a job for process-group teardown, not for the GC.
//
// legacyTTL applies to the one case the lock cannot answer: directories left
// by a daemon predating LockTaskTempDir, which carry no lock file at all
// (removable once this has shipped for a release or two). For those, age is
// the only signal available, so they are reclaimed once the newest mtime
// anywhere inside is older than legacyTTL. Set legacyTTL to 0 to leave them
// alone entirely.
func PruneTaskTempDirs(base string, legacyTTL time.Duration, now time.Time, logger *slog.Logger) (removed int, bytesFreed int64) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return 0, 0 // missing or unreadable base — nothing to prune
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), TaskTempDirPrefix) {
			continue
		}
		dir := filepath.Join(base, e.Name())
		dead, size := taskTempDirIsDead(dir, legacyTTL, now)
		if !dead {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			// Not worth escalating: a directory this cycle could not remove is
			// retried on the next one, and the lock probe still has to
			// re-authorise it. This is the whole reason the fix belongs in the
			// GC rather than in a bounded retry on the task-completion path —
			// here, failing costs nothing and repeats for free.
			if logger != nil {
				logger.Debug("execenv: prune task temp dir failed", "dir", dir, "error", err)
			}
			continue
		}
		removed++
		bytesFreed += size
	}
	return removed, bytesFreed
}

// taskTempDirIsDead reports whether dir's owning execution is provably gone,
// and how many bytes removing it would reclaim.
func taskTempDirIsDead(dir string, legacyTTL time.Duration, now time.Time) (dead bool, size int64) {
	lockPath := filepath.Join(dir, envRootLockFile)
	if _, err := os.Stat(lockPath); err != nil {
		// No lock file: written by a daemon older than this sweep, so nothing
		// ever recorded liveness for it. This also covers the microseconds
		// between os.MkdirTemp and LockTaskTempDir in the current daemon —
		// harmlessly, since legacyTTL is orders of magnitude wider than that
		// window.
		if legacyTTL <= 0 {
			return false, 0
		}
		newest, size := dirStat(dir)
		if newest.IsZero() || now.Sub(newest) <= legacyTTL {
			return false, 0
		}
		return true, size
	}

	lock, err := openLockFile(lockPath)
	if err != nil {
		return false, 0
	}
	locked, err := lockFileExclusiveNonBlocking(lock)
	if err != nil || !locked {
		lock.Close()
		return false, 0
	}
	// We now hold the lock the owner would still be holding, so the kernel has
	// already proven that execution is gone.
	//
	// Release before removing rather than deleting under the held lock: on
	// Windows, removing a file that this process still has open only marks it
	// for deletion, and the parent directory removal then fails until the
	// handle closes. Dropping it first keeps the removal a plain one. Nothing
	// can claim the directory in between — its name is random and its owner is
	// dead — and a second sweep racing us just loses the RemoveAll to ENOENT.
	releaseLockFile(lock)
	_, size = dirStat(dir)
	return true, size
}
