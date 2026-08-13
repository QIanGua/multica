package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

// seedDaemonTaskMarker writes a task-scoped daemon marker into a fresh temp
// directory, backdates it past staleMarkerMinAge, chdirs into a nested
// subdirectory so the CLI's upward walk has to find it, and returns the marker
// path.
func seedDaemonTaskMarker(t *testing.T, body string, age time.Duration) string {
	t.Helper()

	workDir := t.TempDir()
	markerPath := filepath.Join(workDir, execenv.TaskContextMarkerRelPath)
	if err := os.MkdirAll(filepath.Dir(markerPath), 0o755); err != nil {
		t.Fatalf("create marker dir: %v", err)
	}
	if err := os.WriteFile(markerPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	backdated := time.Now().Add(-age)
	if err := os.Chtimes(markerPath, backdated, backdated); err != nil {
		t.Fatalf("backdate marker: %v", err)
	}
	nested := filepath.Join(workDir, "repo", "pkg")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("create nested cwd: %v", err)
	}
	t.Chdir(nested)
	return markerPath
}

func taskScopedMarkerBody() string {
	return `{"managed_by":"` + execenv.TaskContextMarkerManagedBy + `","agent_id":"agent-1","issue_id":"issue-1"}`
}

// stubDaemonHealth points the self-heal probe at a fixed health payload for
// every port, and isolates HOME so knownProfiles() sees only the profiles this
// test creates rather than the developer's real ones.
func stubDaemonHealth(t *testing.T, health map[string]any) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	prev := daemonHealthProbe
	daemonHealthProbe = func(context.Context, int) map[string]any { return health }
	t.Cleanup(func() { daemonHealthProbe = prev })
}

func clearDaemonEnvSignals(t *testing.T) {
	t.Helper()
	t.Setenv("MULTICA_AGENT_ID", "")
	t.Setenv("MULTICA_TASK_ID", "")
	t.Setenv("MULTICA_DAEMON_PORT", "")
	t.Setenv(cli.TaskConfigRootEnv, "")
}

func idleDaemon() map[string]any {
	return map[string]any{"status": "running", "active_task_count": float64(0)}
}

// TestHealStaleMarkerRemovesWhenEveryDaemonReportsIdle is the happy path of
// MUL-6132: a leftover marker in the user's own directory is retired without the
// user doing anything, so the command they ran just works.
func TestHealStaleMarkerRemovesWhenEveryDaemonReportsIdle(t *testing.T) {
	markerPath := seedDaemonTaskMarker(t, taskScopedMarkerBody(), 2*time.Hour)
	stubDaemonHealth(t, idleDaemon())

	if !healStaleDaemonTaskMarker(markerPath) {
		t.Fatal("healStaleDaemonTaskMarker() = false; want true with every daemon idle")
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("marker still present after heal (stat err: %v)", err)
	}
	// The .multica directory existed only to hold the marker.
	if _, err := os.Stat(filepath.Dir(markerPath)); !os.IsNotExist(err) {
		t.Fatalf("empty .multica dir left behind (stat err: %v)", err)
	}
}

// TestHealStaleMarkerRefusesOnUncertainEvidence covers the security rule the
// whole feature turns on: the marker may only be deleted on positive evidence
// that nothing is running. Every other outcome — including a daemon we simply
// cannot reach — must leave it alone, because an unreachable daemon is exactly
// what a crashed one looks like, and a task orphaned by that crash keeps
// running with this marker as its only remaining guard.
func TestHealStaleMarkerRefusesOnUncertainEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		health map[string]any
	}{
		{
			name:   "daemon unreachable",
			health: map[string]any{"status": "stopped"},
		},
		{
			name:   "daemon busy",
			health: map[string]any{"status": "running", "active_task_count": float64(1)},
		},
		{
			name:   "daemon starting and busy",
			health: map[string]any{"status": "starting", "active_task_count": float64(2)},
		},
		{
			name:   "daemon too old to report a task count",
			health: map[string]any{"status": "running"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			markerPath := seedDaemonTaskMarker(t, taskScopedMarkerBody(), 2*time.Hour)
			stubDaemonHealth(t, tc.health)

			if healStaleDaemonTaskMarker(markerPath) {
				t.Fatal("healStaleDaemonTaskMarker() = true; want false on uncertain evidence")
			}
			if _, err := os.Stat(markerPath); err != nil {
				t.Fatalf("marker was removed despite uncertain evidence: %v", err)
			}
		})
	}
}

// TestHealStaleMarkerRefusesFreshMarker keeps the self-heal off the window
// where a daemon reporting zero is claiming a task and about to write this very
// marker.
func TestHealStaleMarkerRefusesFreshMarker(t *testing.T) {
	markerPath := seedDaemonTaskMarker(t, taskScopedMarkerBody(), time.Minute)
	stubDaemonHealth(t, idleDaemon())

	if healStaleDaemonTaskMarker(markerPath) {
		t.Fatal("healStaleDaemonTaskMarker() = true; want false for a marker younger than staleMarkerMinAge")
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("fresh marker was removed: %v", err)
	}
}

// TestHealStaleMarkerRefusesWorkspacesRootMarker protects the node-wide guard.
// The workspaces root marker shares this filename but carries no agent or issue
// — it is permanent by design, and deleting it would silently disarm the
// fail-closed guard for the entire daemon-owned tree.
func TestHealStaleMarkerRefusesWorkspacesRootMarker(t *testing.T) {
	rootMarker := `{"managed_by":"` + execenv.TaskContextMarkerManagedBy + `"}`
	markerPath := seedDaemonTaskMarker(t, rootMarker, 2*time.Hour)
	stubDaemonHealth(t, idleDaemon())

	if healStaleDaemonTaskMarker(markerPath) {
		t.Fatal("healStaleDaemonTaskMarker() = true; want false for the workspaces root marker")
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("workspaces root marker was removed: %v", err)
	}
}

// TestRequireHumanLocalCommandHealsLeftoverMarker is the user-visible outcome:
// the command that used to be permanently refused in this directory now runs.
func TestRequireHumanLocalCommandHealsLeftoverMarker(t *testing.T) {
	markerPath := seedDaemonTaskMarker(t, taskScopedMarkerBody(), 2*time.Hour)
	clearDaemonEnvSignals(t)
	stubDaemonHealth(t, idleDaemon())

	if err := requireHumanLocalCommand("daemon stop"); err != nil {
		t.Fatalf("requireHumanLocalCommand() = %v; want nil after healing the leftover marker", err)
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("marker still present (stat err: %v)", err)
	}
}

// TestRequireHumanLocalCommandNamesMarkerWhenItCannotHeal is the fallback that
// replaces the old bare message. When the marker cannot be proven stale the
// command stays refused — but the user is told which file to look at instead of
// having to read the source to find out.
func TestRequireHumanLocalCommandNamesMarkerWhenItCannotHeal(t *testing.T) {
	markerPath := seedDaemonTaskMarker(t, taskScopedMarkerBody(), 2*time.Hour)
	clearDaemonEnvSignals(t)
	stubDaemonHealth(t, map[string]any{"status": "stopped"})

	err := requireHumanLocalCommand("daemon stop")
	if err == nil {
		t.Fatal("requireHumanLocalCommand() = nil; want refusal when staleness cannot be proven")
	}
	if !strings.Contains(err.Error(), markerPath) {
		t.Fatalf("error should name the marker path %q; got %q", markerPath, err.Error())
	}
	if !strings.Contains(err.Error(), "leftover") {
		t.Fatalf("error should hint it may be a leftover; got %q", err.Error())
	}
	if _, statErr := os.Stat(markerPath); statErr != nil {
		t.Fatalf("marker was removed on the refusal path: %v", statErr)
	}
}

// TestRequireHumanLocalCommandKeepsEnvIdentityUnhealed guards the boundary of
// the self-heal: a real daemon-managed task announces itself through the
// environment, and no file on disk may talk the CLI out of that. These
// invocations keep the original bare refusal and must never trigger a probe or
// a delete.
func TestRequireHumanLocalCommandKeepsEnvIdentityUnhealed(t *testing.T) {
	for _, tc := range []struct{ name, env, value string }{
		{name: "agent id", env: "MULTICA_AGENT_ID", value: "agent-1"},
		{name: "task id", env: "MULTICA_TASK_ID", value: "task-1"},
		{name: "task config root", env: cli.TaskConfigRootEnv, value: "/tmp/task-multica"},
		{name: "daemon port", env: "MULTICA_DAEMON_PORT", value: "20032"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			markerPath := seedDaemonTaskMarker(t, taskScopedMarkerBody(), 2*time.Hour)
			clearDaemonEnvSignals(t)
			t.Setenv(tc.env, tc.value)
			stubDaemonHealth(t, idleDaemon())

			err := requireHumanLocalCommand("daemon stop")
			if err == nil {
				t.Fatal("requireHumanLocalCommand() = nil; want refusal inside a real daemon-managed task")
			}
			if strings.Contains(err.Error(), "leftover") {
				t.Fatalf("env-signalled task context must not be reported as a leftover; got %q", err.Error())
			}
			if _, statErr := os.Stat(markerPath); statErr != nil {
				t.Fatalf("marker was removed inside a real daemon-managed task: %v", statErr)
			}
		})
	}
}

// TestNoDaemonManagedTaskIsLiveRequiresEveryProfile pins the strict rule: a
// single profile whose daemon does not answer makes the whole verdict unknown,
// even when another profile answers idle. A task claimed under one profile can
// be running in a directory a command resolved to a different profile touches.
func TestNoDaemonManagedTaskIsLiveRequiresEveryProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Two named profiles plus the implicit default.
	for _, name := range []string{"alpha", "beta"} {
		dir := filepath.Join(home, ".multica", "profiles", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create profile %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o644); err != nil {
			t.Fatalf("write profile config %s: %v", name, err)
		}
	}

	prev := daemonHealthProbe
	t.Cleanup(func() { daemonHealthProbe = prev })

	// Every port answers idle -> provable.
	daemonHealthProbe = func(context.Context, int) map[string]any { return idleDaemon() }
	if !noDaemonManagedTaskIsLive(context.Background()) {
		t.Fatal("noDaemonManagedTaskIsLive() = false; want true when every profile answers idle")
	}

	// Exactly one port goes silent -> unknown, no deletion allowed.
	silent := healthPortForProfile("beta")
	daemonHealthProbe = func(_ context.Context, port int) map[string]any {
		if port == silent {
			return map[string]any{"status": "stopped"}
		}
		return idleDaemon()
	}
	if noDaemonManagedTaskIsLive(context.Background()) {
		t.Fatal("noDaemonManagedTaskIsLive() = true; want false when a profile's daemon is unreachable")
	}
}
