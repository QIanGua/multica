package execenv

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// gitExcludesFileName is the task-scoped ignore file, written into envRoot —
// daemon scratch — never into the user's repository.
const gitExcludesFileName = ".multica_git_excludes"

// ErrGitExcludesUnprotected is returned when workDir IS inside a git
// repository but the task-scoped excludes could not be established. The caller
// must treat it as fatal and refuse to launch the agent: the whole point of
// this mechanism is that the runtime's own files cannot reach the user's
// history, and an agent running unprotected in a real repository is exactly
// the failure GitHub #7114 reported. Degrading to a warning here would make
// the data-integrity guarantee advisory.
var ErrGitExcludesUnprotected = errors.New("execenv: could not protect the user's repository from the runtime's own files")

// PrepareGitExcludes arranges for the daemon's own sidecars to be invisible to
// the git commands the AGENT runs, for the length of the task, without
// touching a single byte of the user's repository or its git metadata.
//
// It writes a task-scoped ignore file into envRoot and returns the environment
// variables that point the agent's git at it via `core.excludesFile`. Because
// the setting rides in the agent process's environment, it applies to that
// process and its children and to nothing else:
//
//   - The user's own `git status` in the same directory keeps telling them the
//     truth. Their repository is theirs to see.
//   - Sibling worktrees of the same repository are unaffected. An earlier
//     revision of this wrote `.git/info/exclude`, which git resolves to the
//     repository's COMMON git dir — so a pattern written for one in_place task
//     would have hidden identically-named untracked files in every other
//     worktree, including a parallel task's real output, and silently dropped
//     them from that task's `git add -A`.
//   - Nothing survives a crash. There is no on-disk state in the user's repo
//     to roll back, so a killed daemon cannot leave a broad pattern behind
//     quietly hiding files that appear later.
//
// The user's own global excludes are preserved: whatever `core.excludesFile`
// resolved to before is concatenated ahead of our patterns, so overriding the
// setting does not make the agent start seeing files the user globally
// ignores.
//
// Returns nil env with a nil error when workDir is not inside a git repository
// — a local_directory resource may point at a plain folder, and there is
// nothing to protect. Any failure while a git repository IS present is
// returned wrapped in ErrGitExcludesUnprotected.
func PrepareGitExcludes(envRoot, workDir string, paths []string) (map[string]string, error) {
	root, ok := gitRepoRoot(workDir)
	if !ok {
		return nil, nil
	}
	if envRoot == "" {
		return nil, fmt.Errorf("%w: no daemon scratch directory to hold the excludes file", ErrGitExcludesUnprotected)
	}

	patterns, dropped := gitExcludePatterns(root, paths)
	if dropped > 0 {
		// Silently writing a shorter list is how a protection mechanism turns
		// into a no-op nobody notices: the report in #7114 is precisely what
		// an unprotected run looks like.
		return nil, fmt.Errorf("%w: %d of %d sidecar paths could not be expressed relative to %s",
			ErrGitExcludesUnprotected, dropped, len(paths), root)
	}

	body := "# Multica runtime files for the task running in this directory.\n" +
		"# Task-scoped: this file is read only by the agent process, via\n" +
		"# core.excludesFile, and is never written into your repository.\n"
	if inherited, err := inheritedGlobalExcludes(workDir); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrGitExcludesUnprotected, err)
	} else if inherited != "" {
		body += "\n# --- your own global excludes, preserved verbatim ---\n" + inherited + "\n"
	}
	if len(patterns) > 0 {
		body += "\n" + strings.Join(patterns, "\n") + "\n"
	}

	path := filepath.Join(envRoot, gitExcludesFileName)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return nil, fmt.Errorf("%w: write %s: %v", ErrGitExcludesUnprotected, path, err)
	}

	// GIT_CONFIG_COUNT/KEY/VALUE is git's supported way to set config for one
	// invocation tree without a config file (git >= 2.31).
	return map[string]string{
		"GIT_CONFIG_COUNT":   "1",
		"GIT_CONFIG_KEY_0":   "core.excludesFile",
		"GIT_CONFIG_VALUE_0": path,
	}, nil
}

// inheritedGlobalExcludes returns the contents of whatever core.excludesFile
// resolved to before we override it, so the agent keeps ignoring what the user
// globally ignores. A missing or unset file is not an error — most users have
// neither.
func inheritedGlobalExcludes(workDir string) (string, error) {
	current, err := runGitTrimmed(workDir, "config", "--get", "core.excludesFile")
	if err != nil || current == "" {
		// `--get` exits non-zero when the key is unset; fall back to git's
		// documented default location.
		current = defaultGlobalExcludesPath()
		if current == "" {
			return "", nil
		}
	}
	if strings.HasPrefix(current, "~") {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return "", nil
		}
		current = filepath.Join(home, strings.TrimPrefix(current, "~"))
	}
	data, err := os.ReadFile(current)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the global excludes file %s: %w", current, err)
	}
	return strings.TrimRight(string(data), "\n"), nil
}

// defaultGlobalExcludesPath is git's fallback when core.excludesFile is unset:
// $XDG_CONFIG_HOME/git/ignore, or ~/.config/git/ignore.
func defaultGlobalExcludesPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "git", "ignore")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "git", "ignore")
}

// gitRepoRoot returns the repository root containing workDir, canonicalised.
// ok is false when workDir is not inside a git repository, which callers treat
// as "nothing to protect" rather than an error.
func gitRepoRoot(workDir string) (string, bool) {
	out, err := runGitTrimmed(workDir, "rev-parse", "--show-toplevel")
	if err != nil || out == "" {
		return "", false
	}
	return canonicalPath(out), true
}

// canonicalPath resolves symlinks so two spellings of one directory compare
// equal. A local_directory resource keeps the path the user typed, while git
// reports the resolved one — on macOS a /var/... resource against a
// /private/var/... repo root — and comparing them raw made every pattern look
// like it pointed outside the repository (they were then all dropped, leaving
// the run silently unprotected).
func canonicalPath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

// gitExcludePatterns turns absolute sidecar paths into ignore patterns
// anchored at the repository root, sorted so repeated runs on an unchanged
// sidecar set produce identical bytes. dropped counts paths that could not be
// expressed relative to root; the caller treats a non-zero count as a failure
// to protect rather than as a shorter list.
func gitExcludePatterns(root string, paths []string) (patterns []string, dropped int) {
	seen := make(map[string]struct{}, len(paths))
	patterns = make([]string, 0, len(paths))
	for _, p := range paths {
		rel, err := filepath.Rel(root, canonicalPath(p))
		if err != nil || rel == "." || !filepath.IsLocal(rel) {
			dropped++
			continue
		}
		pattern := "/" + filepath.ToSlash(rel)
		// A trailing slash restricts the pattern to directories. Stat rather
		// than trusting the caller: the manifest records files and dirs in one
		// list shape, and a directory pattern that matched a file (or the
		// reverse) would silently fail to exclude anything.
		if info, statErr := os.Stat(p); statErr == nil && info.IsDir() {
			pattern += "/"
		}
		if _, dup := seen[pattern]; dup {
			continue
		}
		seen[pattern] = struct{}{}
		patterns = append(patterns, pattern)
	}
	sort.Strings(patterns)
	return patterns, dropped
}

// GitTrackedFilesUnder returns the absolute paths of files git already tracks
// beneath the given directories, in the repository containing workDir.
//
// This is the state no ignore rule can help with. A repository already
// polluted by GitHub #7114 typically carries committed sidecars: they were
// committed by an earlier run, then deleted by its cleanup, leaving paths that
// are gone from the working tree but still in the index. `os.Lstat` says
// "absent, safe to create", so Prepare writes there again — and because the
// path is tracked, no excludes file can hide it and the next `git add -A`
// stages Multica content once more. That is the reported loop, reproducing on
// exactly the repositories that already suffered it.
//
// Prepare therefore treats these paths as belonging to the user and refuses to
// write them, the same as any other pre-existing path: skills get a
// collision-free alternative directory, and the Multica-only markers degrade
// to absent, which their callers already tolerate.
//
// No candidates, a non-git workDir, or a git failure all yield nothing: this
// hardens a write path that is already conservative, and must never be the
// reason a task cannot start.
func GitTrackedFilesUnder(workDir string, roots []string) map[string]struct{} {
	if len(roots) == 0 {
		return nil
	}
	repoRoot, ok := gitRepoRoot(workDir)
	if !ok {
		return nil
	}
	args := []string{"ls-files", "-z", "--"}
	for _, r := range roots {
		rel, err := filepath.Rel(repoRoot, canonicalPath(r))
		if err != nil || !filepath.IsLocal(rel) {
			continue
		}
		args = append(args, filepath.ToSlash(rel))
	}
	if len(args) == 3 {
		return nil
	}
	// Pathspec-limited so this stays cheap even on a very large repository.
	out, err := runGitStdout(workDir, args...)
	if err != nil {
		return nil
	}

	tracked := make(map[string]struct{})
	for _, entry := range strings.Split(out, "\x00") {
		if entry == "" {
			continue
		}
		tracked[filepath.Join(repoRoot, filepath.FromSlash(entry))] = struct{}{}
	}
	if len(tracked) == 0 {
		return nil
	}
	return tracked
}

// excludablePaths returns the minimal set of paths covering everything the
// manifest records: every created directory that has no created ancestor, plus
// every created file that sits outside all of them.
//
// Minimal matters because each pattern is a line in the ignore file. Recording
// `.grok/skills/multica-a`, `.grok/skills/multica-b`, … when we also created
// `.grok/` itself would write a dozen redundant lines describing one subtree.
// Emitting the shallowest created path is both smaller and more accurate: it
// covers exactly the tree we brought into existence and stops at the boundary
// where the user's own content begins.
func (m sidecarManifest) excludablePaths() []string {
	roots := make([]string, 0, len(m.Dirs))
	for _, d := range m.Dirs {
		if !hasAncestorIn(d, roots) {
			roots = append(roots, d)
		}
	}
	out := append([]string(nil), roots...)
	for _, f := range m.Files {
		if !hasAncestorIn(f, roots) {
			out = append(out, f)
		}
	}
	return out
}

// hasAncestorIn reports whether path lies inside any of the given directories.
func hasAncestorIn(path string, dirs []string) bool {
	for _, d := range dirs {
		rel, err := filepath.Rel(d, path)
		if err != nil || rel == "." {
			continue
		}
		if filepath.IsLocal(rel) {
			return true
		}
	}
	return false
}
