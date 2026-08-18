package execenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The guarantee these tests hold to the wall is behavioural, not textual: a
// sidecar Multica wrote into the user's own repository must be invisible to
// `git status --porcelain` while the task runs, because that is what decides
// whether the agent's own `git add -A` sweeps it into the user's history
// (GitHub #7114). Asserting on the exclude file's bytes alone would pass for
// patterns git does not actually honour.

func excludeFilePath(t *testing.T, repo string) string {
	t.Helper()
	return filepath.Join(repo, ".git", "info", "exclude")
}

func mkSidecarDir(t *testing.T, repo string, rel string) string {
	t.Helper()
	p := filepath.Join(repo, filepath.FromSlash(rel))
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
	writeFile(t, filepath.Join(p, "SKILL.md"), "# skill\n")
	return p
}

func TestExcludeSidecarsHidesThemFromGitStatus(t *testing.T) {
	repo := newTestRepo(t)
	skills := mkSidecarDir(t, repo, ".grok/skills/multica-working-on-issues")
	agentCtx := mkSidecarDir(t, repo, ".agent_context")
	multicaDir := mkSidecarDir(t, repo, ".multica")

	before := gitRun(t, repo, "status", "--porcelain")
	for _, want := range []string{".grok/", ".agent_context/", ".multica/"} {
		if !strings.Contains(before, want) {
			t.Fatalf("precondition: expected %q to be dirty before excluding, got:\n%s", want, before)
		}
	}

	if err := ExcludeSidecarsFromGit(repo, []string{skills, agentCtx, multicaDir}); err != nil {
		t.Fatalf("ExcludeSidecarsFromGit: %v", err)
	}

	after := gitRun(t, repo, "status", "--porcelain")
	if after != "" {
		t.Fatalf("sidecars still visible to git after excluding them:\n%s", after)
	}

	// The decisive assertion: `git add -A` — what agents actually run — must
	// stage nothing.
	gitRun(t, repo, "add", "-A")
	staged := gitRun(t, repo, "diff", "--cached", "--name-only")
	if staged != "" {
		t.Fatalf("git add -A staged Multica runtime files:\n%s", staged)
	}
}

func TestExcludeSidecarsLeavesUserChangesVisible(t *testing.T) {
	repo := newTestRepo(t)
	sidecar := mkSidecarDir(t, repo, ".multica")
	writeFile(t, filepath.Join(repo, "tracked.txt"), "user edit\n")
	writeFile(t, filepath.Join(repo, "brand-new.txt"), "user file\n")

	if err := ExcludeSidecarsFromGit(repo, []string{sidecar}); err != nil {
		t.Fatalf("ExcludeSidecarsFromGit: %v", err)
	}

	status := gitRun(t, repo, "status", "--porcelain")
	for _, want := range []string{"tracked.txt", "brand-new.txt"} {
		if !strings.Contains(status, want) {
			t.Errorf("excluding sidecars hid the user's own change %q; status:\n%s", want, status)
		}
	}
	if strings.Contains(status, ".multica") {
		t.Errorf("sidecar still visible:\n%s", status)
	}
}

// A pattern must be anchored to the repository root, so a directory the user
// happens to name the same way deeper in the tree keeps showing up.
func TestExcludeSidecarsAnchorsPatternsAtRepoRoot(t *testing.T) {
	repo := newTestRepo(t)
	sidecar := mkSidecarDir(t, repo, ".multica")
	mkSidecarDir(t, repo, "docs/.multica")

	if err := ExcludeSidecarsFromGit(repo, []string{sidecar}); err != nil {
		t.Fatalf("ExcludeSidecarsFromGit: %v", err)
	}

	// -uall so git lists files individually; the default collapses a wholly
	// untracked directory to "docs/" and would hide what is being asserted.
	status := gitRun(t, repo, "status", "--porcelain", "-uall")
	if !strings.Contains(status, "docs/.multica") {
		t.Errorf("an unanchored pattern swallowed the user's docs/.multica; status:\n%s", status)
	}
}

// in_place resources may point at a subdirectory of a repository, so patterns
// have to be written relative to the repo root, not to the workdir.
func TestExcludeSidecarsFromRepoSubdirectory(t *testing.T) {
	repo := newTestRepo(t)
	sub := filepath.Join(repo, "packages", "api")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sidecar := mkSidecarDir(t, repo, "packages/api/.multica")

	if err := ExcludeSidecarsFromGit(sub, []string{sidecar}); err != nil {
		t.Fatalf("ExcludeSidecarsFromGit: %v", err)
	}

	body, err := os.ReadFile(excludeFilePath(t, repo))
	if err != nil {
		t.Fatalf("read exclude: %v", err)
	}
	if !strings.Contains(string(body), "/packages/api/.multica/") {
		t.Errorf("pattern not anchored at repo root:\n%s", body)
	}
	if status := gitRun(t, repo, "status", "--porcelain"); strings.Contains(status, ".multica") {
		t.Errorf("sidecar in a subdirectory still visible:\n%s", status)
	}
}

func TestCleanupGitExcludeRestoresUserContentByteForByte(t *testing.T) {
	repo := newTestRepo(t)
	path := excludeFilePath(t, repo)
	original := "# my own excludes\nscratch/\n*.local"
	writeFile(t, path, original)

	sidecar := mkSidecarDir(t, repo, ".multica")
	if err := ExcludeSidecarsFromGit(repo, []string{sidecar}); err != nil {
		t.Fatalf("ExcludeSidecarsFromGit: %v", err)
	}
	if err := CleanupGitExclude(repo); err != nil {
		t.Fatalf("CleanupGitExclude: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read exclude: %v", err)
	}
	if string(got) != original {
		t.Errorf("exclude file not restored byte-for-byte:\n got: %q\nwant: %q", got, original)
	}
}

func TestCleanupGitExcludeRemovesAFileItCreated(t *testing.T) {
	repo := newTestRepo(t)
	path := excludeFilePath(t, repo)
	// git init writes a template exclude; this test is about the state where
	// there was none, so start from a clean slate.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove template exclude: %v", err)
	}

	sidecar := mkSidecarDir(t, repo, ".multica")
	if err := ExcludeSidecarsFromGit(repo, []string{sidecar}); err != nil {
		t.Fatalf("ExcludeSidecarsFromGit: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("exclude file should exist after writing: %v", err)
	}

	if err := CleanupGitExclude(repo); err != nil {
		t.Fatalf("CleanupGitExclude: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("exclude file we created should be gone after cleanup, got err=%v", err)
	}
}

// After cleanup the sidecars must be visible again: the user's repository is
// theirs to see, and a leftover pattern would keep hiding paths forever.
func TestCleanupGitExcludeRestoresVisibility(t *testing.T) {
	repo := newTestRepo(t)
	sidecar := mkSidecarDir(t, repo, ".multica")

	if err := ExcludeSidecarsFromGit(repo, []string{sidecar}); err != nil {
		t.Fatalf("ExcludeSidecarsFromGit: %v", err)
	}
	if err := CleanupGitExclude(repo); err != nil {
		t.Fatalf("CleanupGitExclude: %v", err)
	}

	if status := gitRun(t, repo, "status", "--porcelain"); !strings.Contains(status, ".multica") {
		t.Errorf("sidecar still hidden after cleanup; status:\n%s", status)
	}
}

func TestExcludeSidecarsIsIdempotent(t *testing.T) {
	repo := newTestRepo(t)
	path := excludeFilePath(t, repo)
	sidecar := mkSidecarDir(t, repo, ".multica")

	if err := ExcludeSidecarsFromGit(repo, []string{sidecar}); err != nil {
		t.Fatalf("first write: %v", err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := ExcludeSidecarsFromGit(repo, []string{sidecar}); err != nil {
			t.Fatalf("repeat write %d: %v", i, err)
		}
	}
	again, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(first) != string(again) {
		t.Errorf("repeated writes changed the exclude file:\n first: %q\n again: %q", first, again)
	}
}

func TestExcludeSidecarsNoOpOutsideGitRepo(t *testing.T) {
	dir := t.TempDir()
	sidecar := mkSidecarDir(t, dir, ".multica")

	if err := ExcludeSidecarsFromGit(dir, []string{sidecar}); err != nil {
		t.Errorf("a plain folder must be a no-op, got: %v", err)
	}
	if err := CleanupGitExclude(dir); err != nil {
		t.Errorf("cleanup on a plain folder must be a no-op, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); !os.IsNotExist(err) {
		t.Errorf("must not create a .git directory in a plain folder")
	}
}

func TestExcludeSidecarsNoOpWithoutPaths(t *testing.T) {
	repo := newTestRepo(t)
	path := excludeFilePath(t, repo)
	before, _ := os.ReadFile(path)

	if err := ExcludeSidecarsFromGit(repo, nil); err != nil {
		t.Fatalf("ExcludeSidecarsFromGit: %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Errorf("an empty path list must not touch the exclude file")
	}
}

// A path that is not inside the repository cannot be expressed as a
// repo-relative pattern; dropping it beats guessing and excluding the wrong
// thing.
func TestGitExcludePatternsDropsPathsOutsideRepo(t *testing.T) {
	repo := newTestRepo(t)
	outside := t.TempDir()

	got := gitExcludePatterns(repo, []string{
		filepath.Join(outside, "elsewhere"),
		filepath.Join(repo, ".multica"),
	})
	if len(got) != 1 || got[0] != "/.multica" {
		t.Errorf("expected only the in-repo path, got %v", got)
	}
}

func TestGitExcludePatternsMarksDirectoriesAndSorts(t *testing.T) {
	repo := newTestRepo(t)
	dir := mkSidecarDir(t, repo, ".zzz-dir")
	file := filepath.Join(repo, ".aaa-file")
	writeFile(t, file, "x")

	got := gitExcludePatterns(repo, []string{dir, file})
	want := []string{"/.aaa-file", "/.zzz-dir/"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pattern %d: got %q, want %q (dirs need a trailing slash, output must be sorted)", i, got[i], want[i])
		}
	}
}

// The manifest records every directory it created, including nested ones.
// Emitting all of them would write a dozen redundant lines describing one
// subtree; only the shallowest created path should survive.
func TestExcludablePathsReturnsMinimalCoveringSet(t *testing.T) {
	root := filepath.FromSlash("/repo")
	m := sidecarManifest{
		Dirs: []string{
			filepath.Join(root, ".grok"),
			filepath.Join(root, ".grok", "skills"),
			filepath.Join(root, ".grok", "skills", "multica-squads"),
			filepath.Join(root, ".multica"),
		},
		Files: []string{
			filepath.Join(root, ".grok", "skills", "multica-squads", "SKILL.md"),
			filepath.Join(root, ".multica", "project", "resources.json"),
			filepath.Join(root, "AGENTS.md"),
		},
	}

	got := m.excludablePaths()
	want := map[string]bool{
		filepath.Join(root, ".grok"):     true,
		filepath.Join(root, ".multica"):  true,
		filepath.Join(root, "AGENTS.md"): true,
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want the %d shallowest paths", got, len(want))
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected path %q — it is already covered by an ancestor", p)
		}
	}
}

// When the user already owns the parent directory (their own .grok/skills),
// only the specific directories we created may be excluded, so their siblings
// stay visible.
func TestExcludablePathsKeepsUserOwnedParentsVisible(t *testing.T) {
	root := filepath.FromSlash("/repo")
	m := sidecarManifest{
		Dirs: []string{
			filepath.Join(root, ".grok", "skills", "multica-squads"),
			filepath.Join(root, ".grok", "skills", "multica-mentioning"),
		},
	}

	got := m.excludablePaths()
	if len(got) != 2 {
		t.Fatalf("got %v, want both Multica skill dirs listed individually", got)
	}
	for _, p := range got {
		if filepath.Base(filepath.Dir(p)) != "skills" {
			t.Errorf("path %q escaped up into a directory the user owns", p)
		}
	}
}
