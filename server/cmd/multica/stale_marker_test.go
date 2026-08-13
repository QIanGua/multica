package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

// seedDaemonTaskMarker writes a task-scoped daemon marker into a fresh temp
// directory and chdirs into a nested subdirectory, so the CLI's upward walk has
// to find it the same way it would in a user's repository. Returns the marker
// path.
func seedDaemonTaskMarker(t *testing.T) string {
	t.Helper()

	workDir := t.TempDir()
	markerPath := filepath.Join(workDir, execenv.TaskContextMarkerRelPath)
	if err := os.MkdirAll(filepath.Dir(markerPath), 0o755); err != nil {
		t.Fatalf("create marker dir: %v", err)
	}
	body := `{"managed_by":"` + execenv.TaskContextMarkerManagedBy + `","agent_id":"agent-1","issue_id":"issue-1"}`
	if err := os.WriteFile(markerPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	nested := filepath.Join(workDir, "repo", "pkg")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("create nested cwd: %v", err)
	}
	t.Chdir(nested)
	return markerPath
}

func clearDaemonEnvSignals(t *testing.T) {
	t.Helper()
	t.Setenv("MULTICA_AGENT_ID", "")
	t.Setenv("MULTICA_TASK_ID", "")
	t.Setenv("MULTICA_DAEMON_PORT", "")
	t.Setenv(cli.TaskConfigRootEnv, "")
}

// TestLeftoverMarkerReportedIdenticallyOnBothRefusalPaths is the regression test
// for the split the MUL-6132 review found: only the API path named the marker
// file, so whether a user could act on the refusal depended on which command
// they happened to run first. Both paths must now point at the same file with
// the same remedy.
func TestLeftoverMarkerReportedIdenticallyOnBothRefusalPaths(t *testing.T) {
	markerPath := seedDaemonTaskMarker(t)
	clearDaemonEnvSignals(t)
	t.Setenv("MULTICA_TOKEN", "")
	t.Setenv("MULTICA_SERVER_URL", "http://127.0.0.1:8080")

	humanErr := requireHumanLocalCommand("daemon stop")
	if humanErr == nil {
		t.Fatal("requireHumanLocalCommand() = nil; want refusal on a leftover marker")
	}
	_, apiErr := newAPIClient(testCmd())
	if apiErr == nil {
		t.Fatal("newAPIClient() = nil error; want refusal on a leftover marker")
	}

	for name, err := range map[string]error{"human-local": humanErr, "api": apiErr} {
		if !strings.Contains(err.Error(), markerPath) {
			t.Fatalf("%s error should name the marker path %q; got %q", name, markerPath, err.Error())
		}
		if !strings.Contains(err.Error(), "leftover") {
			t.Fatalf("%s error should hint it may be a leftover; got %q", name, err.Error())
		}
	}

	// The CLI never removes the marker: taking down a fail-closed guard needs a
	// proof the CLI cannot produce, because the daemon's active task count only
	// rises after a claim returns.
	if _, statErr := os.Stat(markerPath); statErr != nil {
		t.Fatalf("CLI must not delete the marker: %v", statErr)
	}
}

// TestLeftoverMarkerNotReportedUnderRealTaskIdentity guards the boundary of the
// leftover treatment. A real daemon-managed task announces itself through the
// environment; those refusals are correct and must not be described to the user
// as a stale file to delete.
func TestLeftoverMarkerNotReportedUnderRealTaskIdentity(t *testing.T) {
	for _, tc := range []struct{ name, env, value string }{
		{name: "agent id", env: "MULTICA_AGENT_ID", value: "agent-1"},
		{name: "task id", env: "MULTICA_TASK_ID", value: "task-1"},
		{name: "task config root", env: cli.TaskConfigRootEnv, value: "/tmp/task-multica"},
		{name: "daemon port", env: "MULTICA_DAEMON_PORT", value: "20032"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			markerPath := seedDaemonTaskMarker(t)
			clearDaemonEnvSignals(t)
			t.Setenv(tc.env, tc.value)

			if got := leftoverDaemonTaskMarkerPath(); got != "" {
				t.Fatalf("leftoverDaemonTaskMarkerPath() = %q; want \"\" under real task identity", got)
			}
			// MULTICA_DAEMON_PORT alone is not task identity, so it does not by
			// itself make a human-local command fail; the others do, and when
			// they do the message must stay the plain one.
			if err := requireHumanLocalCommand("daemon stop"); err != nil {
				if strings.Contains(err.Error(), "leftover") {
					t.Fatalf("env-signalled task context must not be reported as a leftover; got %q", err.Error())
				}
			}
			if _, statErr := os.Stat(markerPath); statErr != nil {
				t.Fatalf("marker must survive: %v", statErr)
			}
		})
	}
}
