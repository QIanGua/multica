package execenv

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// managedBlock is a Multica-owned region inside a file the USER owns. It is
// the shared mechanism behind two round-trips that must both be byte-exact:
//
//   - the runtime brief inside CLAUDE.md / AGENTS.md (runtime_config.go), and
//   - the sidecar patterns inside .git/info/exclude (git_exclude.go).
//
// Both write into a file that may already hold user content, must be
// replaceable in place across repeated runs without growing the file, and
// must be removable afterwards leaving the user's original bytes untouched.
// That is a fiddly contract — trailing-newline handling, half-written blocks,
// stray end markers — and it was already solved once for the runtime config
// (MUL-2753, PR #3438 review). This type is that solution, parameterised on
// the comment syntax, so the second caller inherits the same semantics
// instead of growing a subtly different copy of them.
type managedBlock struct {
	// begin and end delimit the managed region. They must be inert in the
	// target file's syntax: HTML comments for Markdown, `#` comments for
	// gitignore. Changing either is a breaking change for files that already
	// carry the previous markers — bump deliberately.
	begin string
	end   string

	// separator is the fixed byte sequence inserted between pre-existing user
	// content and the block whenever write appends to a file that already
	// exists. It is considered PART of the managed region: cleanup strips it
	// together with the block, so the file rolls back to its exact
	// pre-injection bytes regardless of whether the user's content ended with
	// no newline, one newline, or several. Without a fixed-width separator
	// cleanup would have to renormalise the user's trailing bytes and would
	// leave a subtle but real diff on every run.
	//
	// Its absence before a block is also the signal that distinguishes "we
	// created this file" (remove it on cleanup) from "the file pre-existed"
	// (write the remainder back), which is what preserves the file's
	// existence across the write→cleanup cycle, including for empty and
	// whitespace-only pre-existing files.
	separator string
}

// render wraps body in the block's markers, normalising the body's trailing
// newlines so repeated writes of equal content produce identical bytes.
func (b managedBlock) render(body string) string {
	return b.begin + "\n" + strings.TrimRight(body, "\n") + "\n" + b.end + "\n"
}

// locate finds the [start, end) byte range of the managed block inside
// content. The returned end is one past the block's trailing newline (if any)
// so callers can splice the block out without leaving an orphan blank line.
//
// The end marker is searched for strictly after the begin marker. That
// matters for two malformed cases a naive pair of strings.Index calls would
// mishandle:
//
//   - User content carries a stray end marker (e.g. documentation showing what
//     the wire format looks like) before any begin marker. The naive parser
//     would find that end, reject the block, and append a fresh one — and
//     since the stray end stays put, every later run would append yet another
//     block, growing the file unboundedly.
//   - A previous run crashed between writing begin and end, leaving a
//     half-block. The naive parser would find no end, fall through to the
//     append branch, and stack a new block after the half-block. Treating
//     "begin found, no end after" as "the block runs to EOF" makes the next
//     write replace the half-block in place.
func (b managedBlock) locate(content string) (start, end int, found bool) {
	start = strings.Index(content, b.begin)
	if start < 0 {
		return 0, 0, false
	}
	afterBegin := start + len(b.begin)
	endRel := strings.Index(content[afterBegin:], b.end)
	if endRel < 0 {
		return start, len(content), true
	}
	end = afterBegin + endRel + len(b.end)
	if end < len(content) && content[end] == '\n' {
		end++
	}
	return start, end, true
}

// write puts body into path's managed block without clobbering any
// user-authored content already there. Behaviour by file state:
//
//   - file missing → create it containing only the block, with no leading
//     separator. cleanup detects the absent separator and restores the
//     missing-file state by removing the file outright.
//   - file present (any content, including empty), no block → append
//     separator + block. The separator's bytes are part of the managed
//     region so cleanup can restore the user's pre-write bytes exactly.
//   - file present, block already there → replace the body between the
//     markers in place, so repeated runs don't grow the file. Everything
//     before the block (including a separator established by the first
//     write) is preserved verbatim.
//
// created reports whether the file did not exist and this call brought it
// into being — callers that need to know whether the whole file is Multica's
// (rather than a managed region inside the user's) use it to decide how the
// file should be treated by anything that inspects the directory afterwards.
func (b managedBlock) write(path, body string) (created bool, err error) {
	block := b.render(body)

	existing, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.WriteFile(path, []byte(block), 0o644); err != nil {
			return false, err
		}
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read existing managed file %s: %w", path, err)
	}

	existingStr := string(existing)
	if start, end, ok := b.locate(existingStr); ok {
		// locate already consumed the newline that closed the previous block,
		// so successive runs don't accumulate blank lines around it.
		return false, os.WriteFile(path, []byte(existingStr[:start]+block+existingStr[end:]), 0o644)
	}

	// The separator is unconditional — including for files already ending in
	// two or more newlines — so the byte boundary between user content and the
	// managed region is deterministic, which is what lets cleanup roll back to
	// the user's exact original bytes.
	return false, os.WriteFile(path, []byte(existingStr+b.separator+block), 0o644)
}

// cleanup excises the managed block from path and restores the file to its
// exact pre-write state, byte for byte. It is the second half of write's
// contract: together they must round-trip a user's file across an arbitrary
// number of Multica runs without ever touching a single non-managed byte.
//
// Mirroring write's three states:
//
//   - no block in the file → no-op (nothing was ever written here);
//   - block at the start with no preceding separator → write created the
//     file; remove it outright so the directory listing is byte-identical to
//     the pre-write one;
//   - block preceded by the separator → strip the separator together with the
//     block and write the remainder back verbatim, with NO trailing-newline
//     normalisation and NO TrimSpace-based file-removal heuristic. Both were
//     sources of subtle diff in PR #3438 review.
//
// Missing files and files without a block are no-ops, so cleanup is safe to
// call defensively on any path.
func (b managedBlock) cleanup(path string) error {
	existing, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read managed file %s: %w", path, err)
	}
	existingStr := string(existing)
	start, end, ok := b.locate(existingStr)
	if !ok {
		return nil
	}
	pre := existingStr[:start]
	post := existingStr[end:]

	hadSeparator := strings.HasSuffix(pre, b.separator)
	if hadSeparator {
		pre = pre[:len(pre)-len(b.separator)]
	}
	remainder := pre + post

	if !hadSeparator && remainder == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove managed file %s: %w", path, err)
		}
		return nil
	}
	// An empty remainder here means the user's original file was empty; we
	// still write it (zero-byte file) so the file's existence is preserved.
	return os.WriteFile(path, []byte(remainder), 0o644)
}
