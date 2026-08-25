//go:build unix

package agent

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

// TestRuntimeCommandsGetTheirOwnProcessGroup pins the default that GH #7522
// showed was missing. Before, each backend had to remember to ask for a process
// group and most did not, so a group-wide signal could not reach their CLI at
// all. The group now comes from the one place a runtime process is built, which
// means a backend cannot be launched without it.
func TestRuntimeCommandsGetTheirOwnProcessGroup(t *testing.T) {
	t.Parallel()

	cmd := NewCommand("/bin/sh", nil).exec(context.Background(), "-c", "true")
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("a runtime command must be built with Setpgid so cancellation can signal the whole tree")
	}
}

// TestRuntimeCommandCancellationKillsDescendants is the behaviour the default
// buys, on a command with no backend-specific cancellation logic at all: the
// tool subprocesses an agent spawned must die with it.
//
// os/exec's own Cancel kills the leader alone, which is what left a cancelled
// agent's descendants running. The fake here spawns a grandchild that outlives
// its parent, so killing the leader is not enough to pass.
func TestRuntimeCommandCancellationKillsDescendants(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	pidFile := filepath.Join(tempDir, "pids")
	fakePath := filepath.Join(tempDir, "runtime")
	writeTestExecutable(t, fakePath, []byte("#!/bin/sh\n"+
		`( sleep 300 ) </dev/null >/dev/null 2>&1 &
printf '%s %s\n' "$$" "$!" > "$1"
sleep 300
`))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := NewCommand(fakePath, nil).exec(ctx, pidFile)
	if err := startOwnedProcessTree(cmd, slog.Default()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer releaseProcessGroup(cmd)

	pids := waitForPids(t, pidFile)
	cancel()

	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled process was never reaped")
	}
	for _, pid := range pids {
		waitProcessGone(t, pid)
	}
}
