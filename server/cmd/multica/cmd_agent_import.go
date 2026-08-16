package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// Conflict strategies, reusing the vocabulary `skill import` already
// established (--on-conflict fail|overwrite|rename|skip) so one word means the
// same thing across the CLI.
const (
	agentImportConflictFail      = "fail"
	agentImportConflictOverwrite = "overwrite"
	agentImportConflictRename    = "rename"
	agentImportConflictSkip      = "skip"

	// maxAgentImportRenameAttempts bounds the " (N)" search so a workspace full
	// of same-named agents fails loudly instead of looping.
	maxAgentImportRenameAttempts = 50
)

// agentImportCmd recreates agents from a document written by `agent export`.
// Like export it is a composition over existing endpoints: POST /api/agents to
// create (skill bindings ride along in the same transaction) and
// PUT /api/agents/{id} (+ /skills) to overwrite.
var agentImportCmd = &cobra.Command{
	Use:   "import",
	Short: "Import agents from a JSON document produced by 'agent export'",
	Long: `Create or update agents from a document written by 'multica agent export'.

The document is read from --file or --stdin and applied to the workspace this
CLI is pointed at. Each entry is a COMPLETE statement of an agent's portable
configuration, so an overwrite leaves the target matching the document.

Name collisions are resolved by --on-conflict, matching 'skill import':
  fail       (default) abort before writing anything, listing the collisions
  overwrite  update the existing agent of that name in place
  rename     create a new agent named "<name> (2)"
  skip       leave the existing agent alone and move on

Runtime: --runtime-id chooses where the imported agents run. It may be omitted
only when re-importing into the workspace the document came from, in which case
each agent returns to its recorded runtime. Landing on a different runtime
drops model / thinking_level / service_tier and REQUIRES an explicit --model
(pass --model "" to accept the target runtime default), because the exported
model may not exist there — the same rule as 'agent copy'.

Secrets are not in the document: custom_env, mcp_config and runtime_config are
never exported. Supply fresh values for a single-agent import with the same
secret-safe flags as 'agent create', or set them afterwards with
'multica agent env set' / 'multica agent update --mcp-config'.

Run with --dry-run first to see the plan without writing anything.`,
	Args: exactArgs(0),
	RunE: runAgentImport,
}

func init() {
	agentCmd.AddCommand(agentImportCmd)
	registerAgentImportFlags(agentImportCmd)
}

// registerAgentImportFlags registers every flag runAgentImport reads. Shared
// between init() and the tests so both stay in lockstep.
func registerAgentImportFlags(cmd *cobra.Command) {
	cmd.Flags().String("file", "", "Path to the document to import. Mutually exclusive with --stdin.")
	cmd.Flags().Bool("stdin", false, "Read the document from stdin. Mutually exclusive with --file.")
	cmd.Flags().String("runtime-id", "", "Runtime the imported agents run on. Required unless re-importing into the workspace the document came from.")
	cmd.Flags().String("on-conflict", agentImportConflictFail, "Strategy when an agent of the same name exists: fail, overwrite, rename, or skip")
	cmd.Flags().String("into", "", "Overwrite this specific agent id with the document's single entry, regardless of name. Mutually exclusive with --on-conflict.")
	cmd.Flags().String("name", "", "Override the imported agent's name (single-entry documents only)")
	cmd.Flags().String("model", "", "Model identifier for every imported agent. Required when landing on a different runtime (pass \"\" to accept the target runtime default).")
	cmd.Flags().String("thinking-level", "", "Override thinking level for every imported agent")
	cmd.Flags().String("service-tier", "", "Override Codex service tier for every imported agent")
	cmd.Flags().Int32("max-concurrent-tasks", 6, "Override maximum concurrent tasks for every imported agent")
	cmd.Flags().Bool("no-skills", false, "Do not touch skill bindings: skip them on create, leave existing ones alone on overwrite.")
	cmd.Flags().Bool("dry-run", false, "Report what would be created or updated without writing anything")
	// Secret / machine-local fields are never in the document; these flags
	// provide fresh values, mirroring 'agent create'. Single-entry only.
	cmd.Flags().String("custom-env", "", "Set custom_env on the imported agent as a JSON object (never in the document). Prefer --custom-env-stdin/--custom-env-file for secrets.")
	cmd.Flags().Bool("custom-env-stdin", false, "Read --custom-env from stdin. Mutually exclusive with --custom-env, --custom-env-file, and --stdin.")
	cmd.Flags().String("custom-env-file", "", "Read --custom-env from a file path (suggested mode: 0600). Mutually exclusive with --custom-env and --custom-env-stdin.")
	cmd.Flags().String("mcp-config", "", "Set mcp_config on the imported agent as a JSON object (never in the document). Prefer --mcp-config-stdin/--mcp-config-file for secrets.")
	cmd.Flags().Bool("mcp-config-stdin", false, "Read --mcp-config from stdin. Mutually exclusive with --mcp-config, --mcp-config-file, and --stdin.")
	cmd.Flags().String("mcp-config-file", "", "Read --mcp-config from a file path (suggested mode: 0600). Mutually exclusive with --mcp-config and --mcp-config-stdin.")
	cmd.Flags().String("runtime-config", "", "Set runtime_config on the imported agent as a JSON string (never in the document).")
	cmd.Flags().String("output", "json", "Output format: table or json")
}

// agentImportPlan is one entry's resolved outcome, computed for every entry
// BEFORE any write happens so a --on-conflict fail (or a missing --model) can
// abort the whole run without leaving half a document applied.
type agentImportPlan struct {
	Name string `json:"name"`
	// Action is create, overwrite, or skip.
	Action    string   `json:"action"`
	AgentID   string   `json:"agent_id,omitempty"`
	RuntimeID string   `json:"runtime_id,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`

	entry    agentConfigEntry
	skillIDs []string
	// manageSkills is false under --no-skills, which leaves an overwritten
	// agent's existing bindings untouched.
	manageSkills bool
}

func runAgentImport(cmd *cobra.Command, _ []string) error {
	onConflict, _ := cmd.Flags().GetString("on-conflict")
	switch onConflict {
	case agentImportConflictFail, agentImportConflictOverwrite, agentImportConflictRename, agentImportConflictSkip:
	default:
		return fmt.Errorf("--on-conflict must be one of: fail, overwrite, rename, skip")
	}
	into, _ := cmd.Flags().GetString("into")
	if into != "" && cmd.Flags().Changed("on-conflict") {
		return fmt.Errorf("--into targets one specific agent, so it cannot be combined with --on-conflict")
	}

	var maxConcurrentTasksOverride *int32
	if cmd.Flags().Changed("max-concurrent-tasks") {
		v, _ := cmd.Flags().GetInt32("max-concurrent-tasks")
		if err := validateAgentMaxConcurrentTasksFlag(v); err != nil {
			return err
		}
		maxConcurrentTasksOverride = &v
	}

	// The document and a secret both want stdin; reading one would eat the
	// other. Refuse instead of silently importing an empty env map.
	if fromStdin, _ := cmd.Flags().GetBool("stdin"); fromStdin {
		for _, flag := range []string{"custom-env-stdin", "mcp-config-stdin"} {
			if on, _ := cmd.Flags().GetBool(flag); on {
				return fmt.Errorf("--stdin and --%s both read stdin; pass one of them as a file instead", flag)
			}
		}
	}

	raw, err := readAgentImportDocument(cmd)
	if err != nil {
		return err
	}
	doc, err := decodeAgentConfigDoc(raw)
	if err != nil {
		return err
	}

	// Per-agent overrides only make sense when there is exactly one agent to
	// apply them to; silently applying one secret to five agents would be worse
	// than refusing.
	if len(doc.Agents) > 1 {
		for _, flag := range []string{"name", "into", "custom-env", "custom-env-stdin", "custom-env-file", "mcp-config", "mcp-config-stdin", "mcp-config-file", "runtime-config"} {
			if cmd.Flags().Changed(flag) {
				return fmt.Errorf("--%s applies to a single agent, but the document contains %d; split the document or drop the flag", flag, len(doc.Agents))
			}
		}
	}

	customEnv, hasCustomEnv, err := resolveCustomEnv(cmd)
	if err != nil {
		return err
	}
	mcpConfig, hasMcpConfig, err := resolveMcpConfig(cmd)
	if err != nil {
		return err
	}
	var runtimeConfig any
	hasRuntimeConfig := cmd.Flags().Changed("runtime-config")
	if hasRuntimeConfig {
		v, _ := cmd.Flags().GetString("runtime-config")
		if err := json.Unmarshal([]byte(v), &runtimeConfig); err != nil {
			return fmt.Errorf("--runtime-config must be valid JSON: %w", err)
		}
	}

	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	// A document re-imported into its own workspace can trust the ids it
	// recorded: runtimes still resolve and member allow-list entries still name
	// real people. Anywhere else they are meaningless at best.
	sameWorkspace := doc.Source.WorkspaceID != "" && client.WorkspaceID != "" && doc.Source.WorkspaceID == client.WorkspaceID

	existing, err := listAgentNameIndex(ctx, client)
	if err != nil {
		return err
	}

	skillsByID, skillsByName, err := loadSkillIndex(ctx, client, cmd, doc)
	if err != nil {
		return err
	}

	plans, err := planAgentImport(cmd, doc, planContext{
		onConflict:                 onConflict,
		into:                       into,
		sameWorkspace:              sameWorkspace,
		existing:                   existing,
		skillsByID:                 skillsByID,
		skillsByName:               skillsByName,
		maxConcurrentTasksOverride: maxConcurrentTasksOverride,
	})
	if err != nil {
		return err
	}

	dryRun, _ := cmd.Flags().GetBool("dry-run")
	if dryRun {
		return printAgentImportReport(cmd, plans, true)
	}

	applyErr := applyAgentImport(ctx, cmd, client, plans, agentImportSecrets{
		customEnv:        customEnv,
		hasCustomEnv:     hasCustomEnv,
		mcpConfig:        mcpConfig,
		hasMcpConfig:     hasMcpConfig,
		runtimeConfig:    runtimeConfig,
		hasRuntimeConfig: hasRuntimeConfig,
	})
	// Print the report even on failure: an import is not atomic across agents,
	// and the operator needs to know which entries already landed.
	if printErr := printAgentImportReport(cmd, plans, false); printErr != nil && applyErr == nil {
		return printErr
	}
	return applyErr
}

// readAgentImportDocument reads the document from --file or --stdin. The two
// channels are mutually exclusive, and empty input is an error rather than an
// "imported nothing" success, which almost always means a broken pipe or a
// wrong path.
func readAgentImportDocument(cmd *cobra.Command) ([]byte, error) {
	filePath, _ := cmd.Flags().GetString("file")
	fromStdin, _ := cmd.Flags().GetBool("stdin")

	switch {
	case filePath == "" && !fromStdin:
		return nil, fmt.Errorf("specify the document with --file <path> or --stdin")
	case filePath != "" && fromStdin:
		return nil, fmt.Errorf("--file and --stdin are mutually exclusive; pick one")
	}

	if fromStdin {
		buf, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return nil, fmt.Errorf("read --stdin: %w", err)
		}
		if len(strings.TrimSpace(string(buf))) == 0 {
			return nil, fmt.Errorf("--stdin: empty input")
		}
		return buf, nil
	}

	buf, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("read --file: %w", err)
	}
	return buf, nil
}

// listAgentNameIndex maps every non-archived agent name in the workspace to the
// ids carrying it. Names are not unique server-side, so the value is a slice —
// an ambiguous overwrite target has to be an error, not a coin flip.
func listAgentNameIndex(ctx context.Context, client *cli.APIClient) (map[string][]string, error) {
	if client.WorkspaceID == "" {
		return nil, fmt.Errorf("workspace_id is required: use --workspace-id, set MULTICA_WORKSPACE_ID, or run 'multica config set workspace_id <id>'")
	}

	params := url.Values{}
	params.Set("workspace_id", client.WorkspaceID)

	var agents []map[string]any
	if err := client.GetJSON(ctx, "/api/agents?"+params.Encode(), &agents); err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}

	index := map[string][]string{}
	for _, a := range agents {
		if strVal(a, "archived_at") != "" {
			continue
		}
		name, id := strVal(a, "name"), strVal(a, "id")
		if name == "" || id == "" {
			continue
		}
		index[name] = append(index[name], id)
	}
	return index, nil
}

// loadSkillIndex fetches the target workspace's skill catalog, but only when
// some entry actually needs it. Skill ids do not survive a workspace change, so
// bindings are re-resolved here: by id first, then by unique name.
func loadSkillIndex(ctx context.Context, client *cli.APIClient, cmd *cobra.Command, doc *agentConfigDoc) (map[string]string, map[string][]string, error) {
	if noSkills, _ := cmd.Flags().GetBool("no-skills"); noSkills {
		return nil, nil, nil
	}
	needed := false
	for _, entry := range doc.Agents {
		if len(entry.Skills) > 0 {
			needed = true
			break
		}
	}
	if !needed {
		return nil, nil, nil
	}

	var skills []map[string]any
	if err := client.GetJSON(ctx, "/api/skills", &skills); err != nil {
		return nil, nil, fmt.Errorf("list skills: %w", err)
	}

	byID := map[string]string{}
	byName := map[string][]string{}
	for _, s := range skills {
		id, name := strVal(s, "id"), strVal(s, "name")
		if id == "" {
			continue
		}
		byID[id] = name
		if name != "" {
			byName[name] = append(byName[name], id)
		}
	}
	return byID, byName, nil
}

type planContext struct {
	onConflict                 string
	into                       string
	sameWorkspace              bool
	existing                   map[string][]string
	skillsByID                 map[string]string
	skillsByName               map[string][]string
	maxConcurrentTasksOverride *int32
}

// planAgentImport resolves every entry to a concrete action, returning an error
// for anything that must abort the run before the first write.
func planAgentImport(cmd *cobra.Command, doc *agentConfigDoc, pc planContext) ([]agentImportPlan, error) {
	runtimeFlag, _ := cmd.Flags().GetString("runtime-id")
	nameOverride, hasNameOverride := "", cmd.Flags().Changed("name")
	if hasNameOverride {
		nameOverride, _ = cmd.Flags().GetString("name")
		if strings.TrimSpace(nameOverride) == "" {
			return nil, fmt.Errorf("--name must not be empty")
		}
	}
	noSkills, _ := cmd.Flags().GetBool("no-skills")

	// taken tracks names claimed during THIS run as well, so two renamed
	// entries in one document cannot both land on "<name> (2)".
	taken := map[string]bool{}
	for name := range pc.existing {
		taken[name] = true
	}

	var conflicts []string
	plans := make([]agentImportPlan, 0, len(doc.Agents))

	for _, entry := range doc.Agents {
		if hasNameOverride {
			entry.Name = strings.TrimSpace(nameOverride)
		}

		plan := agentImportPlan{Name: entry.Name, manageSkills: !noSkills}

		sourceRuntimeID := ""
		if entry.Origin != nil {
			sourceRuntimeID = entry.Origin.RuntimeID
		}
		switch {
		case runtimeFlag != "":
			plan.RuntimeID = runtimeFlag
		case pc.sameWorkspace && sourceRuntimeID != "":
			plan.RuntimeID = sourceRuntimeID
		default:
			return nil, fmt.Errorf("--runtime-id is required: %q was exported from another workspace (or without a runtime), so its recorded runtime does not apply here", entry.Name)
		}

		// Runtime-specific tuning does not travel to a runtime it was not chosen
		// for — the same contract as 'agent copy'.
		if plan.RuntimeID != sourceRuntimeID {
			if !cmd.Flags().Changed("model") {
				return nil, fmt.Errorf("importing %q onto a different runtime requires --model, because the exported model may not exist there; pass --model \"\" to accept the target runtime default", entry.Name)
			}
			entry.Model, entry.ThinkingLevel, entry.ServiceTier = "", "", ""
		}
		if cmd.Flags().Changed("model") {
			entry.Model, _ = cmd.Flags().GetString("model")
		}
		if cmd.Flags().Changed("thinking-level") {
			entry.ThinkingLevel, _ = cmd.Flags().GetString("thinking-level")
		}
		if cmd.Flags().Changed("service-tier") {
			entry.ServiceTier, _ = cmd.Flags().GetString("service-tier")
		}
		if pc.maxConcurrentTasksOverride != nil {
			entry.MaxConcurrentTasks = pc.maxConcurrentTasksOverride
		}

		entry.InvocationTargets, entry.PermissionMode, plan.Warnings = resolveImportedPermission(entry, pc.sameWorkspace, plan.Warnings)

		if !noSkills {
			plan.skillIDs, plan.Warnings = resolveImportedSkills(entry, pc.skillsByID, pc.skillsByName, plan.Warnings)
		}

		switch {
		case pc.into != "":
			plan.Action = agentImportConflictOverwrite
			plan.AgentID = pc.into
		case !taken[entry.Name]:
			plan.Action = "create"
			taken[entry.Name] = true
		case pc.onConflict == agentImportConflictFail:
			conflicts = append(conflicts, entry.Name)
		case pc.onConflict == agentImportConflictSkip:
			plan.Action = "skip"
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("an agent named %q already exists; skipped", entry.Name))
		case pc.onConflict == agentImportConflictOverwrite:
			ids := pc.existing[entry.Name]
			if len(ids) != 1 {
				return nil, fmt.Errorf("cannot overwrite %q: %d agents in this workspace share that name; pass --into <agent-id> to choose one", entry.Name, len(ids))
			}
			plan.Action = agentImportConflictOverwrite
			plan.AgentID = ids[0]
		case pc.onConflict == agentImportConflictRename:
			renamed, err := freeAgentName(entry.Name, taken)
			if err != nil {
				return nil, err
			}
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("an agent named %q already exists; imported as %q", entry.Name, renamed))
			entry.Name, plan.Name = renamed, renamed
			plan.Action = "create"
			taken[renamed] = true
		}

		if len(conflicts) > 0 {
			continue
		}
		// Appended last so the note that explains the ACTION (renamed, skipped)
		// reads before the standing reminders about what the document omits.
		plan.Warnings = append(plan.Warnings, excludedFieldWarnings(entry)...)
		plan.entry = entry
		plans = append(plans, plan)
	}

	if len(conflicts) > 0 {
		return nil, fmt.Errorf("agents already exist in this workspace: %s; use --on-conflict overwrite to replace them, rename to import copies, or skip to leave them alone", strings.Join(conflicts, ", "))
	}
	return plans, nil
}

// resolveImportedPermission decides which invocation allow-list survives the
// import. A workspace target means "everyone here" and travels anywhere; member
// and team targets name ids from the source workspace, so carrying them across
// would grant invocation rights to whoever happens to hold that id — they are
// dropped, and an agent left with no targets at all falls back to private
// rather than to a public_to mode nobody can actually use.
func resolveImportedPermission(entry agentConfigEntry, sameWorkspace bool, warnings []string) ([]agentConfigTarget, string, []string) {
	if entry.PermissionMode != "public_to" || sameWorkspace {
		return entry.InvocationTargets, entry.PermissionMode, warnings
	}

	kept := make([]agentConfigTarget, 0, len(entry.InvocationTargets))
	dropped := 0
	for _, t := range entry.InvocationTargets {
		if t.TargetType == "workspace" {
			kept = append(kept, agentConfigTarget{TargetType: "workspace"})
			continue
		}
		dropped++
	}
	if dropped > 0 {
		warnings = append(warnings, fmt.Sprintf("%d invocation allow-list entr(y/ies) named members of the source workspace and were dropped", dropped))
	}
	if len(kept) == 0 {
		warnings = append(warnings, "no invocation allow-list entry survived the workspace change; imported as private")
		return nil, "private", warnings
	}
	return kept, entry.PermissionMode, warnings
}

// resolveImportedSkills re-resolves exported skill bindings against the target
// workspace: by id when the id still exists there, otherwise by a unique name
// match. Anything unresolved is reported rather than silently dropped, because
// a missing skill is a missing capability at claim time.
func resolveImportedSkills(entry agentConfigEntry, byID map[string]string, byName map[string][]string, warnings []string) ([]string, []string) {
	ids := make([]string, 0, len(entry.Skills))
	seen := map[string]bool{}
	for _, s := range entry.Skills {
		resolved := ""
		if s.ID != "" {
			if _, ok := byID[s.ID]; ok {
				resolved = s.ID
			}
		}
		if resolved == "" && s.Name != "" {
			switch matches := byName[s.Name]; len(matches) {
			case 0:
			case 1:
				resolved = matches[0]
			default:
				warnings = append(warnings, fmt.Sprintf("skill %q matches %d skills in this workspace; binding skipped", s.Name, len(matches)))
				continue
			}
		}
		if resolved == "" {
			warnings = append(warnings, fmt.Sprintf("skill %q does not exist in this workspace; binding skipped", skillLabel(s)))
			continue
		}
		if seen[resolved] {
			continue
		}
		seen[resolved] = true
		ids = append(ids, resolved)
	}
	return ids, warnings
}

func skillLabel(s agentConfigSkill) string {
	if s.Name != "" {
		return s.Name
	}
	return s.ID
}

// excludedFieldWarnings restates what the export deliberately left out, so an
// operator does not discover a missing API key the first time the agent runs.
func excludedFieldWarnings(entry agentConfigEntry) []string {
	if entry.Excluded == nil {
		return nil
	}
	var warnings []string
	if entry.Excluded.HasCustomEnv {
		if entry.Excluded.CustomEnvKeyCount > 0 {
			warnings = append(warnings, fmt.Sprintf("source agent had %d custom_env variable(s); set them with 'multica agent env set'", entry.Excluded.CustomEnvKeyCount))
		} else {
			warnings = append(warnings, "source agent had custom_env variables; set them with 'multica agent env set'")
		}
	}
	if entry.Excluded.HasMcpConfig {
		warnings = append(warnings, "source agent had an mcp_config; set it with 'multica agent update --mcp-config-file'")
	}
	if entry.Excluded.HasRuntimeConfig {
		warnings = append(warnings, "source agent had a runtime_config; set it with 'multica agent update --runtime-config'")
	}
	return warnings
}

// freeAgentName finds the first unused "<base> (N)" name, mirroring the numeric
// suffix `skill import --on-conflict rename` uses.
func freeAgentName(base string, taken map[string]bool) (string, error) {
	for suffix := 2; suffix < maxAgentImportRenameAttempts+2; suffix++ {
		candidate := fmt.Sprintf("%s (%d)", base, suffix)
		if !taken[candidate] {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not find an available name for %q after %d attempts; pass --name", base, maxAgentImportRenameAttempts)
}

type agentImportSecrets struct {
	customEnv        map[string]string
	hasCustomEnv     bool
	mcpConfig        json.RawMessage
	hasMcpConfig     bool
	runtimeConfig    any
	hasRuntimeConfig bool
}

// applyAgentImport executes the plans in document order, stopping at the first
// failure. Entries already applied stay applied — the report tells the operator
// exactly how far it got.
func applyAgentImport(ctx context.Context, cmd *cobra.Command, client *cli.APIClient, plans []agentImportPlan, secrets agentImportSecrets) error {
	for i := range plans {
		plan := &plans[i]
		switch plan.Action {
		case "skip":
			continue
		case "create":
			body := agentImportCreateBody(*plan)
			applyImportSecrets(body, secrets, true)
			var result map[string]any
			if err := client.PostJSON(ctx, "/api/agents", body, &result); err != nil {
				return fmt.Errorf("create agent %q: %w", plan.Name, err)
			}
			plan.AgentID = strVal(result, "id")
		case agentImportConflictOverwrite:
			body := agentImportUpdateBody(*plan)
			applyImportSecrets(body, secrets, false)
			var result map[string]any
			if err := client.PutJSON(ctx, "/api/agents/"+url.PathEscape(plan.AgentID), body, &result); err != nil {
				return fmt.Errorf("update agent %q: %w", plan.Name, err)
			}
			if plan.manageSkills {
				var skills json.RawMessage
				skillBody := map[string]any{"skill_ids": plan.skillIDs}
				if err := client.PutJSON(ctx, "/api/agents/"+url.PathEscape(plan.AgentID)+"/skills", skillBody, &skills); err != nil {
					return fmt.Errorf("set skills for agent %q: %w", plan.Name, err)
				}
			}
			// custom_env has no path through the generic update endpoint; it is
			// deliberately gated behind the dedicated audited one.
			if secrets.hasCustomEnv {
				var envResult map[string]any
				envBody := map[string]any{"custom_env": secrets.customEnv}
				if err := client.PutJSON(ctx, "/api/agents/"+url.PathEscape(plan.AgentID)+"/env", envBody, &envResult); err != nil {
					return fmt.Errorf("set env for agent %q: %w", plan.Name, err)
				}
			}
		}
	}
	return nil
}

func agentImportCreateBody(plan agentImportPlan) map[string]any {
	entry := plan.entry
	body := map[string]any{
		"name":         entry.Name,
		"runtime_id":   plan.RuntimeID,
		"description":  entry.Description,
		"instructions": entry.Instructions,
		"custom_args":  entry.CustomArgs,
	}
	if entry.AvatarURL != "" {
		body["avatar_url"] = entry.AvatarURL
	}
	if entry.Model != "" {
		body["model"] = entry.Model
	}
	if entry.ThinkingLevel != "" {
		body["thinking_level"] = entry.ThinkingLevel
	}
	if entry.ServiceTier != "" {
		body["service_tier"] = entry.ServiceTier
	}
	if entry.MaxConcurrentTasks != nil {
		body["max_concurrent_tasks"] = *entry.MaxConcurrentTasks
	}
	if entry.PermissionMode != "" {
		body["permission_mode"] = entry.PermissionMode
		body["invocation_targets"] = importTargetsBody(entry.InvocationTargets)
	}
	if plan.manageSkills && len(plan.skillIDs) > 0 {
		body["skill_ids"] = plan.skillIDs
	}
	return body
}

// agentImportUpdateBody sends the whole portable field set, including empty
// values, so an overwrite leaves the agent matching the document instead of
// keeping values the document does not carry. Fields the document genuinely
// omits (avatar, an out-of-range historical concurrency) are left untouched.
func agentImportUpdateBody(plan agentImportPlan) map[string]any {
	entry := plan.entry
	body := map[string]any{
		"name":           entry.Name,
		"runtime_id":     plan.RuntimeID,
		"description":    entry.Description,
		"instructions":   entry.Instructions,
		"custom_args":    entry.CustomArgs,
		"model":          entry.Model,
		"thinking_level": entry.ThinkingLevel,
		"service_tier":   entry.ServiceTier,
	}
	if entry.AvatarURL != "" {
		body["avatar_url"] = entry.AvatarURL
	}
	if entry.MaxConcurrentTasks != nil {
		body["max_concurrent_tasks"] = *entry.MaxConcurrentTasks
	}
	if entry.PermissionMode != "" {
		body["permission_mode"] = entry.PermissionMode
		body["invocation_targets"] = importTargetsBody(entry.InvocationTargets)
	}
	return body
}

func importTargetsBody(targets []agentConfigTarget) []map[string]any {
	out := make([]map[string]any, 0, len(targets))
	for _, t := range targets {
		target := map[string]any{"target_type": t.TargetType}
		if t.TargetID != "" {
			target["target_id"] = t.TargetID
		}
		out = append(out, target)
	}
	return out
}

// applyImportSecrets folds the explicitly supplied secret / machine-local
// values into a request body. custom_env rides along only on create; the update
// endpoint rejects it by design, so the overwrite path calls /env separately.
func applyImportSecrets(body map[string]any, secrets agentImportSecrets, create bool) {
	if create && secrets.hasCustomEnv {
		body["custom_env"] = secrets.customEnv
	}
	if secrets.hasMcpConfig {
		body["mcp_config"] = secrets.mcpConfig
	}
	if secrets.hasRuntimeConfig {
		body["runtime_config"] = secrets.runtimeConfig
	}
}

func printAgentImportReport(cmd *cobra.Command, plans []agentImportPlan, dryRun bool) error {
	output, _ := cmd.Flags().GetString("output")
	if output == "json" {
		return cli.PrintJSON(cmd.OutOrStdout(), map[string]any{
			"dry_run": dryRun,
			"agents":  plans,
		})
	}

	rows := make([][]string, 0, len(plans))
	for _, p := range plans {
		action := p.Action
		if dryRun {
			action = "would " + action
		}
		rows = append(rows, []string{p.Name, action, p.AgentID, strings.Join(p.Warnings, "; ")})
	}
	cli.PrintTable(cmd.OutOrStdout(), []string{"NAME", "ACTION", "AGENT_ID", "NOTES"}, rows)
	return nil
}
