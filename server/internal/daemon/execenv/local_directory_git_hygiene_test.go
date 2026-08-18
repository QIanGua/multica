package execenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// End-to-end for the in_place half of GitHub #7114: run a real Prepare against
// a real git repository and prove the daemon's own files cannot reach the
// user's history through the agent's `git add -A`.
//
// The leaf functions have unit tests in git_exclude_test.go. This one exists
// because the bug was never in a leaf — it was in the wiring: the sidecars were
// written, the cleanup was correct, and nothing connected the two to git in
// between.
func TestPrepareInPlaceKeepsRepositoryCleanForGitAddAll(t *testing.T) {
	repo := newTestRepo(t)
	workspacesRoot := t.TempDir()

	env, err := Prepare(PrepareParams{
		WorkspacesRoot: workspacesRoot,
		WorkspaceID:    "ws-gh7114-001",
		TaskID:         "aaaaaaaa-1111-2222-3333-444444444444",
		AgentName:      "Test Agent",
		Provider:       "grok",
		LocalWorkDir:   repo,
		Task: TaskContextForEnv{
			IssueID: "11111111-2222-3333-4444-555555555555",
			AgentID: "99999999-8888-7777-6666-555555555555",
		},
	}, testLogger())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if len(env.SidecarPaths) == 0 {
		t.Fatal("Prepare wrote sidecars into the user's repo but reported none to hide from git")
	}
	if dirty := gitRun(t, repo, "status", "--porcelain"); dirty == "" {
		t.Fatalf("precondition: expected Prepare to dirty the repo, got a clean tree")
	}

	// What the daemon does with SidecarPaths once Prepare hands them over.
	gitEnvVars, err := PrepareGitExcludes(env.RootDir, repo, env.SidecarPaths)
	if err != nil {
		t.Fatalf("PrepareGitExcludes: %v", err)
	}

	if status := gitEnv(t, repo, gitEnvVars, "status", "--porcelain"); status != "" {
		t.Errorf("runtime files still visible to the agent's git:\n%s", status)
	}
	gitEnv(t, repo, gitEnvVars, "add", "-A")
	if staged := gitEnv(t, repo, gitEnvVars, "diff", "--cached", "--name-only"); staged != "" {
		t.Errorf("the agent's `git add -A` staged Multica runtime files:\n%s", staged)
	}

	// And the repository is handed back exactly as it was found.
	gitRun(t, repo, "reset")
	if err := CleanupSidecars(env.RootDir); err != nil {
		t.Fatalf("CleanupSidecars: %v", err)
	}
	if status := gitRun(t, repo, "status", "--porcelain"); status != "" {
		t.Errorf("repository not restored to its pre-task state:\n%s", status)
	}
}

// Elon review, must-fix 2, end to end. This is the state a repository already
// hit by #7114 is in: sidecars were committed by an earlier run and then
// deleted by its cleanup, so the paths are gone from disk but still tracked.
// Prepare must not write there again — no ignore rule can hide a modification
// to a tracked file, so doing so would re-arm the exact loop reported.
func TestPrepareInPlaceDoesNotRewriteCommittedThenDeletedSidecars(t *testing.T) {
	repo := newTestRepo(t)

	// Reproduce the damage the old behaviour left behind.
	firstEnv, err := Prepare(PrepareParams{
		WorkspacesRoot: t.TempDir(),
		WorkspaceID:    "ws-gh7114-004",
		TaskID:         "dddddddd-1111-2222-3333-444444444444",
		AgentName:      "Test Agent",
		Provider:       "grok",
		LocalWorkDir:   repo,
		Task: TaskContextForEnv{
			IssueID: "11111111-2222-3333-4444-555555555555",
			AgentID: "99999999-8888-7777-6666-555555555555",
		},
	}, testLogger())
	if err != nil {
		t.Fatalf("first Prepare: %v", err)
	}
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-m", "the bug: runtime files committed")
	if err := CleanupSidecars(firstEnv.RootDir); err != nil {
		t.Fatalf("CleanupSidecars: %v", err)
	}
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-m", "the bug: runtime files deleted again")

	// A later task on the same repository.
	secondEnv, err := Prepare(PrepareParams{
		WorkspacesRoot: t.TempDir(),
		WorkspaceID:    "ws-gh7114-004",
		TaskID:         "eeeeeeee-1111-2222-3333-444444444444",
		AgentName:      "Test Agent",
		Provider:       "grok",
		LocalWorkDir:   repo,
		Task: TaskContextForEnv{
			IssueID: "11111111-2222-3333-4444-555555555555",
			AgentID: "99999999-8888-7777-6666-555555555555",
		},
	}, testLogger())
	if err != nil {
		t.Fatalf("second Prepare: %v", err)
	}
	gitEnvVars, err := PrepareGitExcludes(secondEnv.RootDir, repo, secondEnv.SidecarPaths)
	if err != nil {
		t.Fatalf("PrepareGitExcludes: %v", err)
	}

	gitEnv(t, repo, gitEnvVars, "add", "-A")
	if staged := gitEnv(t, repo, gitEnvVars, "diff", "--cached", "--name-only"); staged != "" {
		t.Errorf("the injection loop is still armed on an already-polluted repo; staged:\n%s", staged)
	}
}

// A local_directory resource may point at a plain folder. Prepare must still
// work there, and must not turn it into a git repository.
func TestPrepareInPlaceOnNonGitFolder(t *testing.T) {
	userDir := t.TempDir()
	workspacesRoot := t.TempDir()

	env, err := Prepare(PrepareParams{
		WorkspacesRoot: workspacesRoot,
		WorkspaceID:    "ws-gh7114-002",
		TaskID:         "bbbbbbbb-1111-2222-3333-444444444444",
		AgentName:      "Test Agent",
		Provider:       "grok",
		LocalWorkDir:   userDir,
		Task: TaskContextForEnv{
			IssueID: "11111111-2222-3333-4444-555555555555",
			AgentID: "99999999-8888-7777-6666-555555555555",
		},
	}, testLogger())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := PrepareGitExcludes(t.TempDir(), userDir, env.SidecarPaths); err != nil {
		t.Errorf("excluding in a non-git folder must be a no-op, got: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(userDir, ".git")); !os.IsNotExist(statErr) {
		t.Errorf("must not create a git repository in the user's plain folder (stat err: %v)", statErr)
	}
}

// Cloud workdirs are daemon scratch wiped wholesale by the GC, and worktree
// mode deletes the worktree; neither should carry the in_place bookkeeping.
func TestPrepareCloudWorkdirReportsNoSidecarPaths(t *testing.T) {
	workspacesRoot := t.TempDir()

	env, err := Prepare(PrepareParams{
		WorkspacesRoot: workspacesRoot,
		WorkspaceID:    "ws-gh7114-003",
		TaskID:         "cccccccc-1111-2222-3333-444444444444",
		AgentName:      "Test Agent",
		Provider:       "grok",
		Task: TaskContextForEnv{
			IssueID: "11111111-2222-3333-4444-555555555555",
			AgentID: "99999999-8888-7777-6666-555555555555",
		},
	}, testLogger())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(env.SidecarPaths) != 0 {
		t.Errorf("cloud workdir should report no sidecar paths, got %v", env.SidecarPaths)
	}
}

// The runtime brief must not be written into a repository's tracked AGENTS.md
// on the in_place path for a runtime that can receive it inline. This asserts
// the execenv half of that contract: nothing writes the file unless asked.
func TestBuildRuntimeBriefDoesNotTouchTheRepository(t *testing.T) {
	repo := newTestRepo(t)
	agentsMD := filepath.Join(repo, "AGENTS.md")
	writeFile(t, agentsMD, "# Project rules\n\nUse tabs.\n")
	gitRun(t, repo, "add", "AGENTS.md")
	gitRun(t, repo, "commit", "-m", "add agents.md")

	brief := BuildRuntimeBrief("grok", TaskContextForEnv{
		IssueID: "11111111-2222-3333-4444-555555555555",
		AgentID: "99999999-8888-7777-6666-555555555555",
	})
	if strings.TrimSpace(brief) == "" {
		t.Fatal("BuildRuntimeBrief returned nothing; the agent would run with no brief at all")
	}
	if status := gitRun(t, repo, "status", "--porcelain"); status != "" {
		t.Errorf("building the brief modified the repository:\n%s", status)
	}
}
