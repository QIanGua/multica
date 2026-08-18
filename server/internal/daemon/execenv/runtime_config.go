package execenv

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/multica-ai/multica/server/pkg/agent"
)

// runtimeMarkerBegin and runtimeMarkerEnd delimit the Multica-managed brief
// inside the runtime config file (CLAUDE.md / AGENTS.md). The
// markers exist so writeRuntimeConfigFile can:
//
//   - preserve user-authored content in the same file (the user's repo may
//     already ship a CLAUDE.md / AGENTS.md when the agent is pointed at a
//     local_directory project resource),
//   - replace the brief idempotently on subsequent runs in the same workdir
//     instead of appending duplicate copies, and
//   - leave a precise excision target for a future cleanup pass.
//
// HTML comments are used so the markers are inert in every Markdown renderer
// and harmless when fed to the agent as instructions. Changing the marker
// text is a breaking change for any file that already carries the previous
// markers — bump deliberately.
const (
	runtimeMarkerBegin = "<!-- BEGIN MULTICA-RUNTIME (auto-managed; do not edit) -->"
	runtimeMarkerEnd   = "<!-- END MULTICA-RUNTIME -->"

	// runtimeManagedSeparator is the fixed separator inserted between any
	// pre-existing user content and the marker block whenever Inject
	// appends to a file that already exists. The separator is considered
	// part of the managed region: Cleanup strips it together with the
	// block, so the file rolls back to its exact pre-injection bytes
	// regardless of whether the user file ended with no newline, one
	// newline, or multiple trailing newlines. Without a fixed-width
	// separator the cleanup path would have to renormalise the user's
	// trailing bytes and would leave a subtle but real diff every run
	// (see MUL-2753 review on PR #3438).
	//
	// Cleanup distinguishes "file we created" (no managed separator
	// precedes the block — write a missing file from scratch) from "file
	// that pre-existed" (managed separator precedes the block) so the
	// file's existence is preserved exactly across the inject→cleanup
	// cycle, including empty / whitespace-only pre-existing files.
	runtimeManagedSeparator = "\n\n"
)

// runtimeGOOS is the host-platform string used by buildMetaSkillContent and
// BuildCommentReplyInstructions to emit Windows-specific guidance. Defaults
// to runtime.GOOS; tests override it to exercise the cross-platform branches
// deterministically without having to run on every target OS.
var runtimeGOOS = runtime.GOOS

// sanitizeNameForBriefMarkdown turns a possibly-multiline display name into a
// single-line, plain-text token that is safe to embed inside markdown inline
// constructs (e.g. `**%s**`) in the agent brief. The brief is loaded as
// trusted instructions, so user-controlled name fields must not be able to
// introduce headings, lists, or close the surrounding bold span.
//
// CR/LF and other whitespace control bytes collapse to a single space; other
// C0 controls and DEL are dropped; markdown structural characters that have
// meaning in inline context (`*`, `_`, “ ` “, `\`, `[`, `]`, `<`) are
// backslash-escaped. Trailing whitespace is trimmed.
func sanitizeNameForBriefMarkdown(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	prevSpace := false
	for _, r := range name {
		switch {
		case r == '\r' || r == '\n' || r == '\t' || r == '\v' || r == '\f':
			if !prevSpace && b.Len() > 0 {
				b.WriteByte(' ')
				prevSpace = true
			}
		case r < 0x20 || r == 0x7f:
			continue
		case r == '*' || r == '_' || r == '`' || r == '\\' || r == '[' || r == ']' || r == '<':
			b.WriteByte('\\')
			b.WriteRune(r)
			prevSpace = false
		default:
			b.WriteRune(r)
			prevSpace = false
		}
	}
	return strings.TrimSpace(b.String())
}

// sanitizeEmailForBrief returns the email verbatim when it is safe to embed
// inline in the brief, or "" when it carries a character a real address never
// has (whitespace, control chars, or a markdown-break risk). Unlike
// sanitizeNameForBriefMarkdown it does NOT backslash-escape markdown specials:
// an agent may want to match the initiator's address exactly, and escaping
// `_`/`+` would corrupt it, while a valid email can't contain a newline to
// inject a heading anyway. Emails are validated at signup, so this is
// defense-in-depth, not the primary guard. See MUL-2645.
func sanitizeEmailForBrief(email string) string {
	email = strings.TrimSpace(email)
	if email == "" || !strings.Contains(email, "@") {
		return ""
	}
	for _, r := range email {
		if r < 0x20 || r == 0x7f || r == ' ' || r == '\\' || r == '`' || r == '*' || r == '<' || r == '>' || r == '[' || r == ']' {
			return ""
		}
	}
	return email
}

// formatProjectResource renders a single resource as a human-readable bullet.
// Unknown resource types fall back to a JSON-encoded ref so the agent can
// still read what the user attached. New resource types should add a case
// here AND in the API validator (handler/project_resource.go).
func formatProjectResource(r ProjectResourceForEnv) string {
	label := r.Label
	switch r.ResourceType {
	case "github_repo":
		var payload struct {
			URL               string `json:"url"`
			DefaultBranchHint string `json:"default_branch_hint,omitempty"`
			Ref               string `json:"ref,omitempty"`
		}
		_ = json.Unmarshal(r.ResourceRef, &payload)
		out := fmt.Sprintf("**GitHub repo**: %s", payload.URL)
		details := make([]string, 0, 2)
		if payload.Ref != "" {
			details = append(details, fmt.Sprintf("checkout ref: `%s`", payload.Ref))
		}
		if payload.DefaultBranchHint != "" {
			details = append(details, fmt.Sprintf("default branch hint: `%s`", payload.DefaultBranchHint))
		}
		if len(details) > 0 {
			out += " (" + strings.Join(details, ", ") + ")"
		}
		if label != "" {
			out += " — " + label
		}
		return out
	default:
		ref := string(r.ResourceRef)
		if ref == "" {
			ref = "{}"
		}
		out := fmt.Sprintf("**%s**: `%s`", r.ResourceType, ref)
		if label != "" {
			out += " — " + label
		}
		return out
	}
}

// InjectRuntimeConfig writes the meta skill content into the runtime-specific
// config file so the agent discovers its environment through its native mechanism.
//
// For Claude:   writes {workDir}/CLAUDE.md  (skills discovered natively from .claude/skills/)
// For CodeBuddy: writes {workDir}/CODEBUDDY.md  (CodeBuddy's native memory filename; skills discovered natively from .codebuddy/skills/)
// For Codex:    writes {workDir}/AGENTS.md  (skills discovered natively via CODEX_HOME)
// For Copilot:  writes {workDir}/AGENTS.md  (skills discovered natively from .github/skills/)
// For OpenCode: writes {workDir}/AGENTS.md  (skills discovered natively from .opencode/skills/)
// For DevEco Code: writes {workDir}/AGENTS.md  (skills discovered natively from .deveco/skills/)
// For OpenClaw: writes {workDir}/AGENTS.md  (skills discovered natively from {workDir}/skills/ via per-task openclaw-config.json that pins agents.defaults.workspace)
// For Hermes:   writes {workDir}/AGENTS.md  (skills discovered natively from a per-task HERMES_HOME/skills seeded by the daemon; see hermes_home.go)
// For Pi:       writes {workDir}/AGENTS.md  (skills discovered natively from .pi/skills/)
// For Oh-My-Pi (omp): writes {workDir}/AGENTS.md  (omp is a pi fork; skills discovered from .omp/skills/)
// For Cursor:   writes {workDir}/AGENTS.md  (skills discovered natively from .cursor/skills/)
// For Kimi:        writes {workDir}/AGENTS.md  (Kimi Code CLI reads AGENTS.md natively; skills auto-discovered from project skills dirs)
// For Reasonix:    writes {workDir}/AGENTS.md  (Reasonix reads AGENTS.md and .reasonix/skills/ natively)
// For DSH:         writes {workDir}/AGENTS.md  (DSH reads AGENTS.md and .dsh/skills/ natively)
// For Kiro:        writes {workDir}/AGENTS.md  (Kiro CLI reads AGENTS.md natively; skills auto-discovered from project skills dirs)
// For Qoder/Qoder CN: writes {workDir}/AGENTS.md  (skills discovered from .qoder/skills/; user-level roots are unaffected)
// For Antigravity: writes {workDir}/AGENTS.md  (agy CLI reads AGENTS.md natively; skills discovered natively from .agents/skills/ — see https://antigravity.google/docs/gcli-migration)
// For Traecli:     writes {workDir}/AGENTS.md  (traecli reads .trae/rules/ not AGENTS.md, so the brief is delivered inline via providerNeedsInlineSystemPrompt; the file is written for parity/visibility only)
// For Grok:        writes {workDir}/AGENTS.md  (Grok Build CLI reads AGENTS.md natively from the workdir)
// For Qwen:        writes {workDir}/QWEN.md (Qwen Code's native context file; it also reads AGENTS.md, but QWEN.md avoids cross-runtime ambiguity)
func InjectRuntimeConfig(workDir, provider string, ctx TaskContextForEnv) (string, error) {
	content, _, err := InjectRuntimeConfigCreated(workDir, provider, ctx)
	return content, err
}

// InjectRuntimeConfigCreated is InjectRuntimeConfig plus whether the config
// file had to be created because it did not already exist.
//
// The in_place local_directory flow needs that distinction. A config file we
// created is wholly Multica's and is an untracked new file in the user's
// repository, so it can — and must — be kept out of their git status for the
// run. A config file that pre-existed is the user's own, usually tracked, and
// nothing in git's ignore machinery applies to it; only the marker block
// inside it is ours, and CleanupRuntimeConfig is what reverses that.
func InjectRuntimeConfigCreated(workDir, provider string, ctx TaskContextForEnv) (content string, created bool, err error) {
	content = buildMetaSkillContent(provider, ctx)
	path := runtimeConfigPath(workDir, provider)
	if path == "" {
		// Unknown provider — skip config injection, prompt-only mode.
		return content, false, nil
	}
	created, err = writeRuntimeConfigFileCreated(path, content)
	return content, created, err
}

// BuildRuntimeBrief returns the runtime brief for a provider WITHOUT writing
// it anywhere. Used by the in_place local_directory flow for runtimes that can
// receive the brief inline in the turn, where writing it to disk would mean
// appending a Multica-managed block to a file the user's project versions as
// its own instructions (GitHub #7114).
func BuildRuntimeBrief(provider string, ctx TaskContextForEnv) string {
	return buildMetaSkillContent(provider, ctx)
}

// RuntimeConfigPath exposes the config file a provider's brief would be
// written to, or "" when the provider has no file-based target.
func RuntimeConfigPath(workDir, provider string) string {
	return runtimeConfigPath(workDir, provider)
}

// runtimeConfigPath returns the absolute path to the runtime config file that
// InjectRuntimeConfig writes for the given provider, or "" when the provider
// has no file-based config target. Centralising the mapping keeps Inject /
// Cleanup in lockstep — both paths consult the same table so a new provider
// added to one side cannot drift past the other.
func runtimeConfigPath(workDir, provider string) string {
	// Built-in runtime identities (e.g. "omp") inherit their config file
	// from their protocol family — resolve the family from the descriptor
	// and delegate to the family's switch case. This avoids hardcoding
	// "AGENTS.md" for every descriptor; a compatible runtime on Claude,
	// CodeBuddy, or Qwen would otherwise write the wrong file.
	if desc, ok := agent.BuiltinRuntimeByID(provider); ok {
		return runtimeConfigPath(workDir, desc.ProtocolFamily)
	}
	switch provider {
	case "claude":
		return filepath.Join(workDir, "CLAUDE.md")
	case "codebuddy":
		// CodeBuddy Code's native memory file is CODEBUDDY.md, not
		// CLAUDE.md — see https://www.codebuddy.ai/docs/cli/codebuddy-dir
		// ("CODEBUDDY.md / .codebuddy/CODEBUDDY.md — Project-level memory
		// file"). CodeBuddy only reads CLAUDE.md if the user manually
		// migrates/symlinks it in.
		return filepath.Join(workDir, "CODEBUDDY.md")
	case "qwen":
		return filepath.Join(workDir, "QWEN.md")
	case "codex", "copilot", "opencode", "deveco", "openclaw", "hermes", "pi", "cursor", "kimi", "reasonix", "dsh", "kiro", "antigravity", "qoder", "qoderclicn", "traecli", "grok", "qwenpaw":
		return filepath.Join(workDir, "AGENTS.md")
	default:
		return ""
	}
}

// runtimeConfigBlock is the managed region Multica owns inside the runtime
// config file. The surrounding user content — a repository's own CLAUDE.md or
// AGENTS.md, which is exactly what a local_directory task points the agent at
// — is never touched; see managedBlock for the full write/cleanup contract.
var runtimeConfigBlock = managedBlock{
	begin:     runtimeMarkerBegin,
	end:       runtimeMarkerEnd,
	separator: runtimeManagedSeparator,
}

// writeRuntimeConfigFile writes the Multica runtime brief into path's managed
// block without clobbering user-authored content already present. Returns
// whether the file had to be created (it did not exist before this call), which
// the local_directory flow uses to decide whether the file is wholly Multica's
// and therefore safe to keep out of the user's git status entirely.
//
// The previous implementation called os.WriteFile unconditionally, which
// silently truncated a repository's CLAUDE.md / AGENTS.md the first time the
// agent was pointed at the user's own directory via the local_directory
// project resource flow. See MUL-2753.
func writeRuntimeConfigFileCreated(path, brief string) (created bool, err error) {
	return runtimeConfigBlock.write(path, brief)
}

// writeRuntimeConfigFile is the error-only form, for callers that do not care
// whether the file pre-existed.
func writeRuntimeConfigFile(path, brief string) error {
	_, err := writeRuntimeConfigFileCreated(path, brief)
	return err
}

// CleanupRuntimeConfig excises the Multica marker block from the runtime
// config file for the given provider and restores the file to its exact
// pre-injection state, byte for byte.
//
// Required for the local_directory flow (WorkDir is the user's own repo):
// without this pass, a manual `claude` / `codex` run started by the user
// inside the same directory after a Multica task would pick up the stale
// brief and act on the previous task's issue id, trigger comment id, and
// reply rules. Cloud workspace runs never trigger this pollution because
// their workdir is daemon scratch that the GC loop deletes wholesale; the
// daemon skips this Cleanup on those workdirs.
//
// Missing files, unknown providers, and files without a marker block are
// no-ops — Cleanup is safe to call defensively.
func CleanupRuntimeConfig(workDir, provider string) error {
	path := runtimeConfigPath(workDir, provider)
	if path == "" {
		return nil
	}
	return runtimeConfigBlock.cleanup(path)
}

// buildMetaSkillContent generates the meta skill markdown that teaches the
// agent about the Multica runtime environment and available CLI tools.
//
// The brief is assembled by buildMetaSkillContentSlim (runtime_config_sections.go),
// which applies kind-driven section gating + per-section prose compression.
// This used to be gated behind the `runtime_brief_slim` feature flag against a
// legacy verbose brief; the flag has been retired (MUL-4297) and the slim brief
// is now the only path.
func buildMetaSkillContent(provider string, ctx TaskContextForEnv) string {
	return buildMetaSkillContentSlim(provider, ctx)
}
