package execenv

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// gitExcludeBlock is the managed region Multica owns inside the repository's
// .git/info/exclude. `#` is gitignore's comment syntax, so the markers are
// inert to git and to anything else reading the file.
//
// info/exclude rather than .gitignore, deliberately: .gitignore is a TRACKED
// file in virtually every repository, so writing our patterns there would
// commit a Multica-authored change to the user's project — the very class of
// pollution this file exists to prevent, and it would land in the diff of the
// next `git add -A` exactly like the sidecars do. info/exclude lives inside
// .git/, is never tracked, never appears in a diff, and never reaches a remote.
var gitExcludeBlock = managedBlock{
	begin:     "# BEGIN MULTICA-RUNTIME (auto-managed; do not edit)",
	end:       "# END MULTICA-RUNTIME",
	separator: "\n\n",
}

// ExcludeSidecarsFromGit hides the daemon's own sidecar files from git inside
// the repository that contains workDir, by writing them as patterns into the
// repo's .git/info/exclude.
//
// This exists for the in_place local_directory flow, where WorkDir IS the
// user's repository and the sidecars Prepare writes (.multica/,
// .agent_context/, the runtime's skills tree) are therefore untracked files
// sitting in the user's own working tree for the length of the run. Cleanup
// removes them afterwards, but "afterwards" is too late: the agent itself
// routinely runs `git add -A` mid-task and commits them into the user's
// project. Excluding them closes that window at its only reliable point —
// git's own view of the tree — instead of hoping no commit happens first.
// See GitHub #7114.
//
// Scope and safety:
//
//   - Only paths this task actually created are excluded. The caller passes
//     them from the sidecar manifest, which records a path only after
//     verifying it did NOT pre-exist, so a pattern can never hide a file that
//     is the user's.
//   - Patterns are anchored to the repository root with a leading slash, so
//     `/.multica/` cannot also swallow a `docs/.multica/` the user owns.
//   - A workDir that is not inside a git repository is a no-op, as is an empty
//     path list. Both are ordinary: local_directory resources may point at a
//     plain folder.
//
// Not used for worktree mode. A linked worktree resolves info/exclude to the
// repository's COMMON git dir — the user's own — so writing there would change
// what `git status` hides in the user's checkout for a file we did not put in
// their checkout. Worktree mode keeps its existing guarantee instead: the
// sidecars are removed before Finalize commits anything.
func ExcludeSidecarsFromGit(workDir string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	root, excludePath, ok := gitExcludeTarget(workDir)
	if !ok {
		return nil
	}

	patterns := gitExcludePatterns(root, paths)
	if len(patterns) == 0 {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return fmt.Errorf("create git info dir: %w", err)
	}
	body := "# Multica runtime files for the task running in this directory.\n" +
		"# Removed automatically when the task finishes.\n" +
		strings.Join(patterns, "\n")
	if _, err := gitExcludeBlock.write(excludePath, body); err != nil {
		return fmt.Errorf("write git exclude %s: %w", excludePath, err)
	}
	return nil
}

// CleanupGitExclude removes the managed block from the repository's
// .git/info/exclude, restoring the file to its exact pre-task bytes (and
// removing it outright when Multica created it).
//
// Safe to call defensively: a workDir outside any git repository, a missing
// exclude file, and a file without the managed block are all no-ops.
func CleanupGitExclude(workDir string) error {
	_, excludePath, ok := gitExcludeTarget(workDir)
	if !ok {
		return nil
	}
	if err := gitExcludeBlock.cleanup(excludePath); err != nil {
		return fmt.Errorf("clean git exclude %s: %w", excludePath, err)
	}
	return nil
}

// gitExcludeTarget resolves the repository root containing workDir and the
// path of its info/exclude file. ok is false when workDir is not inside a git
// repository, which callers treat as "nothing to do" rather than an error.
//
// --git-common-dir rather than --git-dir: the two differ inside a linked
// worktree, and only the common dir holds the info/exclude git actually reads.
func gitExcludeTarget(workDir string) (root, excludePath string, ok bool) {
	out, err := runGitTrimmed(workDir, "rev-parse", "--show-toplevel", "--git-common-dir")
	if err != nil {
		return "", "", false
	}
	lines := strings.Split(out, "\n")
	if len(lines) < 2 {
		return "", "", false
	}
	root = strings.TrimSpace(lines[0])
	gitDir := strings.TrimSpace(lines[1])
	if root == "" || gitDir == "" {
		return "", "", false
	}
	if !filepath.IsAbs(gitDir) {
		// git resolves a relative --git-common-dir against the directory it
		// ran in, which -C pinned to workDir.
		gitDir = filepath.Join(workDir, gitDir)
	}
	return root, filepath.Join(gitDir, "info", "exclude"), true
}

// gitExcludePatterns turns absolute sidecar paths into gitignore patterns
// anchored at the repository root, sorted for deterministic file contents so
// repeated runs on an unchanged sidecar set rewrite identical bytes.
//
// A path that resolves outside root is dropped rather than emitted: it cannot
// be expressed as a repo-relative pattern, and guessing would risk excluding
// the wrong thing.
func gitExcludePatterns(root string, paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	patterns := make([]string, 0, len(paths))
	for _, p := range paths {
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == "." || !filepath.IsLocal(rel) {
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
	return patterns
}

// excludablePaths returns the minimal set of paths covering everything the
// manifest records: every created directory that has no created ancestor, plus
// every created file that sits outside all of them.
//
// Minimal matters because each pattern is a line in the user's info/exclude.
// Recording `.grok/skills/multica-a`, `.grok/skills/multica-b`, … when we also
// created `.grok/` itself would write a dozen redundant lines describing one
// subtree. Emitting the shallowest created path is both smaller and more
// accurate: it excludes exactly the tree we brought into existence and stops
// at the boundary where the user's own content begins.
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
