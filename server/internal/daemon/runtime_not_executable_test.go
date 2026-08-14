package daemon

import (
	"context"
	"fmt"
	"io/fs"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/agent"
)

// stubNotExecutableProbe makes every version probe fail the way the OS fails a
// file it will not execute — an npm placeholder stub whose postinstall was
// blocked (MUL-6164) — wrapped exactly as detectCLIVersion wraps it, so the
// daemon sees the same error shape it sees in the field.
func stubNotExecutableProbe(t *testing.T, path string) {
	t.Helper()
	orig := detectAgentVersion
	t.Cleanup(func() { detectAgentVersion = orig })
	detectAgentVersion = func(_ context.Context, p string) (string, error) {
		enoexec := &fs.PathError{Op: "fork/exec", Path: p, Err: syscall.ENOEXEC}
		return "", fmt.Errorf("detect version for %s: %w", p, agent.ExplainExecError(enoexec))
	}
}

// stubConfirmWindow collapses the confirmation window so a second probe round
// counts as confirmation immediately. Two rounds are still required.
func stubConfirmWindow(t *testing.T, window time.Duration) {
	t.Helper()
	orig := notExecutableConfirmWindow
	t.Cleanup(func() { notExecutableConfirmWindow = orig })
	notExecutableConfirmWindow = window
}

func notExecutableDaemon(t *testing.T) *Daemon {
	t.Helper()
	stubProbeRetry(t, time.Millisecond, time.Millisecond)
	stubNotExecutableProbe(t, "/fake/claude")
	d := freshDaemon("")
	d.cfg.Agents = map[string]AgentEntry{"claude": {Path: "/fake/claude", Command: "claude"}}
	return d
}

// A CLI that cannot exec is a fact about the file on disk, so it must end up on
// the demotable side of the probe rather than the transient one — that is what
// takes the runtime offline instead of leaving it online to claim tasks it
// cannot run. The first sighting still waits: an installer overwriting the bin
// entry in place is briefly indistinguishable from a broken install.
func TestDetectBuiltinRuntimes_NotExecutableDemotesOnlyAfterConfirmation(t *testing.T) {
	stubConfirmWindow(t, 0)
	d := notExecutableDaemon(t)

	runtimes, demotable, unavailable := d.detectBuiltinRuntimes(context.Background())
	if len(runtimes) != 0 {
		t.Fatalf("registered %v, want none: the CLI cannot run", runtimes)
	}
	if _, ok := demotable["claude"]; ok {
		t.Errorf("first sighting demoted %v, want it held for confirmation", demotable)
	}
	if _, ok := unavailable["claude"]; !ok {
		t.Errorf("first sighting = %v, want claude treated as transient for one round", unavailable)
	}

	_, demotable, unavailable = d.detectBuiltinRuntimes(context.Background())
	if _, ok := unavailable["claude"]; ok {
		t.Errorf("second sighting = %v, want claude condemned, not held again", unavailable)
	}
	reason, ok := demotable["claude"]
	if !ok {
		t.Fatalf("second sighting did not condemn claude: %v", demotable)
	}
	// The reason is what /health shows and what the demotion logs, so it has to
	// carry the repair instructions, not just the errno.
	for _, want := range []string{"not executable", "postinstall"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason is missing %q, so the user still cannot act on it:\n%s", want, reason)
		}
	}
}

// Two probes inside the window are two samples of the same instant as far as
// this verdict is concerned — an in-place install can span them.
func TestDetectBuiltinRuntimes_NotExecutableWaitsOutTheWindow(t *testing.T) {
	stubConfirmWindow(t, time.Hour)
	d := notExecutableDaemon(t)

	for round := 1; round <= 3; round++ {
		_, demotable, _ := d.detectBuiltinRuntimes(context.Background())
		if _, ok := demotable["claude"]; ok {
			t.Fatalf("round %d demoted inside the confirmation window", round)
		}
	}
}

// The window is measured from the FIRST sighting, and a healthy probe resets it
// — otherwise an unrelated failure months later would inherit a stale clock and
// demote on its first sighting.
func TestConfirmNotExecutable_WindowStartsAtFirstSighting(t *testing.T) {
	stubConfirmWindow(t, time.Minute)
	d := freshDaemon("")
	start := time.Now()

	if d.confirmNotExecutable("claude", start) {
		t.Fatal("first sighting confirmed the verdict on its own")
	}
	if d.confirmNotExecutable("claude", start.Add(59*time.Second)) {
		t.Error("confirmed before the window elapsed")
	}
	if !d.confirmNotExecutable("claude", start.Add(time.Minute)) {
		t.Error("did not confirm after the window elapsed")
	}

	d.clearNotExecutable("claude")
	if d.confirmNotExecutable("claude", start.Add(time.Hour)) {
		t.Error("a recovered provider must start a fresh window, not inherit the old one")
	}
}

// The user-visible half of the feature: once the verdict is confirmed, the
// provider's runtimes actually go offline instead of staying online to claim
// tasks that die at fork/exec — and they come back on their own after the
// repair, without a daemon restart.
func TestRefreshAgentVersions_TakesNotExecutableRuntimeOfflineAndBack(t *testing.T) {
	stubConfirmWindow(t, 0)
	fx := newVersionRefreshFixture(t)
	d := fx.daemon

	// npm blocks the postinstall on the next `npm i -g`: same path, same
	// executable bit, a placeholder stub where the binary used to be.
	fx.setProbeErr(func(path string, _ int) error {
		enoexec := &fs.PathError{Op: "fork/exec", Path: path, Err: syscall.ENOEXEC}
		return fmt.Errorf("detect version for %s: %w", path, agent.ExplainExecError(enoexec))
	})

	d.refreshAgentVersions(context.Background()) // first sighting: held
	for _, workspaceID := range []string{"ws-1", "ws-2"} {
		if got := registeredProviders(t, d, workspaceID); len(got) != 1 {
			t.Fatalf("%s providers = %v, want codex still online during confirmation", workspaceID, got)
		}
	}

	d.refreshAgentVersions(context.Background()) // confirmed: demote
	for _, workspaceID := range []string{"ws-1", "ws-2"} {
		if got := registeredProviders(t, d, workspaceID); len(got) != 0 {
			t.Errorf("%s providers = %v, want none: the CLI cannot run, so the runtime must not keep claiming tasks", workspaceID, got)
		}
	}
	if reason := d.skippedAgentsSnapshot()["codex"]; !strings.Contains(reason, "not executable") {
		t.Errorf("/health reason = %q, want it to say why the runtime went offline", reason)
	}

	// The user runs the repair. Nothing restarts; the converge path re-registers.
	fx.setProbeErr(nil)
	d.convergeRuntimeRegistrations(context.Background())
	for _, workspaceID := range []string{"ws-1", "ws-2"} {
		if got := registeredProviders(t, d, workspaceID); len(got) != 1 || got[0] != "codex" {
			t.Errorf("%s providers after repair = %v, want [codex] back without a restart", workspaceID, got)
		}
	}
}

// A provider that probes OK again clears its pending verdict, which is what
// makes recovery automatic: no runtime + no hold puts it back in
// providersMissingRuntimes for the converge path to register.
func TestDetectBuiltinRuntimes_HealthyProbeClearsPendingVerdict(t *testing.T) {
	stubConfirmWindow(t, 0)
	stubProbeRetry(t, time.Millisecond, time.Millisecond)
	d := freshDaemon("")
	d.cfg.Agents = map[string]AgentEntry{"claude": {Path: "/fake/claude", Command: "claude"}}

	broken := true
	orig := detectAgentVersion
	t.Cleanup(func() { detectAgentVersion = orig })
	detectAgentVersion = func(_ context.Context, p string) (string, error) {
		if broken {
			enoexec := &fs.PathError{Op: "fork/exec", Path: p, Err: syscall.ENOEXEC}
			return "", fmt.Errorf("detect version for %s: %w", p, agent.ExplainExecError(enoexec))
		}
		return "2.1.231", nil
	}

	d.detectBuiltinRuntimes(context.Background()) // first sighting: clock starts
	broken = false
	if runtimes, _, _ := d.detectBuiltinRuntimes(context.Background()); len(runtimes) != 1 {
		t.Fatalf("repaired CLI registered %v, want one runtime", runtimes)
	}
	broken = true
	if _, demotable, _ := d.detectBuiltinRuntimes(context.Background()); len(demotable) != 0 {
		t.Errorf("a fresh failure demoted immediately (%v): the healthy probe should have reset the clock", demotable)
	}
}
