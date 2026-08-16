package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// The portable agent-configuration document shared by `agent export` and
// `agent import`. It carries exactly the field set `agent copy` already treats
// as portable (MUL-5279) — one definition of "portable agent configuration" in
// the product, not two: name/description/instructions/avatar, the runtime
// tuning fields (model, thinking_level, service_tier, custom_args,
// max_concurrent_tasks), invocation permission, and workspace skill bindings.
//
// Secret and machine-local fields — custom_env, mcp_config, runtime_config —
// are NEVER written to the document. GET redacts or masks them anyway, so an
// export could only ever record masked junk, and a config file that silently
// carries credentials is the wrong artifact to hand around. What the document
// records instead is their PRESENCE (Excluded), so an import can tell the
// operator "this agent expects 3 env vars you still have to supply".
const (
	agentConfigDocKind    = "multica.agent.config"
	agentConfigDocVersion = 1
)

type agentConfigDoc struct {
	Kind    string             `json:"kind"`
	Version int                `json:"version"`
	Source  agentConfigSource  `json:"source"`
	Agents  []agentConfigEntry `json:"agents"`
}

// agentConfigSource identifies where the document came from. WorkspaceID is
// load-bearing on import: an import back into the SAME workspace can reuse the
// recorded runtime and member allow-list, while a cross-workspace import must
// not — those ids mean nothing (or, for member targets, something wrong) over
// there.
type agentConfigSource struct {
	WorkspaceID string `json:"workspace_id,omitempty"`
	ExportedAt  string `json:"exported_at,omitempty"`
}

// agentConfigEntry is a COMPLETE statement of one agent's portable config, not
// a patch. Fields an agent always has are serialized even when empty, so an
// overwrite import leaves the target matching the document instead of keeping
// stale values the document does not mention. Only genuinely optional data
// (avatar, an out-of-range historical concurrency, a private agent's empty
// allow-list, provenance) is omitted when absent, and those are left untouched
// on overwrite.
type agentConfigEntry struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Instructions string `json:"instructions"`
	// AvatarURL travels verbatim, like the web Duplicate action: an
	// `emoji:<glyph>` avatar is portable anywhere, while an uploaded image URL
	// keeps pointing at the source server's asset.
	AvatarURL          string              `json:"avatar_url,omitempty"`
	Model              string              `json:"model"`
	ThinkingLevel      string              `json:"thinking_level"`
	ServiceTier        string              `json:"service_tier"`
	CustomArgs         []string            `json:"custom_args"`
	MaxConcurrentTasks *int32              `json:"max_concurrent_tasks,omitempty"`
	PermissionMode     string              `json:"permission_mode"`
	InvocationTargets  []agentConfigTarget `json:"invocation_targets,omitempty"`
	Skills             []agentConfigSkill  `json:"skills"`
	Origin             *agentConfigOrigin  `json:"origin,omitempty"`
	Excluded           *agentConfigOmitted `json:"excluded,omitempty"`
}

type agentConfigTarget struct {
	TargetType string `json:"target_type"`
	TargetID   string `json:"target_id,omitempty"`
}

// agentConfigSkill records both id and name so an import can bind by id in the
// source workspace and fall back to a name match anywhere else.
type agentConfigSkill struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// agentConfigOrigin is informational provenance. Import reads RuntimeID from
// it only when the document came from the target workspace.
type agentConfigOrigin struct {
	AgentID     string `json:"agent_id,omitempty"`
	RuntimeID   string `json:"runtime_id,omitempty"`
	RuntimeMode string `json:"runtime_mode,omitempty"`
}

// agentConfigOmitted describes the secret / machine-local fields the export
// deliberately left out, so an import can warn about what the new agent still
// needs before it can run.
type agentConfigOmitted struct {
	HasCustomEnv      bool `json:"has_custom_env,omitempty"`
	CustomEnvKeyCount int  `json:"custom_env_key_count,omitempty"`
	HasMcpConfig      bool `json:"has_mcp_config,omitempty"`
	HasRuntimeConfig  bool `json:"has_runtime_config,omitempty"`
}

// buildAgentConfigEntry projects a GET /api/agents/<id> response onto the
// portable field set. Anything not on this whitelist is dropped by
// construction, which is what keeps secrets out of the document even if the
// API response later grows new fields.
func buildAgentConfigEntry(src map[string]any) agentConfigEntry {
	entry := agentConfigEntry{
		Name:           strVal(src, "name"),
		Description:    strVal(src, "description"),
		Instructions:   strVal(src, "instructions"),
		AvatarURL:      strVal(src, "avatar_url"),
		Model:          strVal(src, "model"),
		ThinkingLevel:  strVal(src, "thinking_level"),
		ServiceTier:    strVal(src, "service_tier"),
		PermissionMode: strVal(src, "permission_mode"),
		CustomArgs:     []string{},
		Skills:         []agentConfigSkill{},
	}

	if args, ok := src["custom_args"].([]any); ok {
		for _, a := range args {
			if s, ok := a.(string); ok {
				entry.CustomArgs = append(entry.CustomArgs, s)
			}
		}
	}

	// Historical rows may predate the 1-50 invariant; an out-of-range value is
	// omitted so the import lands on the server default instead of turning into
	// a 400 — the same guard `agent copy` applies.
	if v, ok := copiedAgentMaxConcurrentTasks(src["max_concurrent_tasks"]); ok {
		entry.MaxConcurrentTasks = &v
	}

	if targets, ok := src["invocation_targets"].([]any); ok {
		for _, t := range targets {
			m, ok := t.(map[string]any)
			if !ok {
				continue
			}
			targetType := strVal(m, "target_type")
			if targetType == "" {
				continue
			}
			entry.InvocationTargets = append(entry.InvocationTargets, agentConfigTarget{
				TargetType: targetType,
				TargetID:   strVal(m, "target_id"),
			})
		}
	}

	if skills, ok := src["skills"].([]any); ok {
		for _, s := range skills {
			m, ok := s.(map[string]any)
			if !ok {
				continue
			}
			id, name := strVal(m, "id"), strVal(m, "name")
			if id == "" && name == "" {
				continue
			}
			entry.Skills = append(entry.Skills, agentConfigSkill{ID: id, Name: name})
		}
	}

	if origin := (agentConfigOrigin{
		AgentID:     strVal(src, "id"),
		RuntimeID:   strVal(src, "runtime_id"),
		RuntimeMode: strVal(src, "runtime_mode"),
	}); origin != (agentConfigOrigin{}) {
		entry.Origin = &origin
	}

	if omitted := agentConfigOmittedFrom(src); omitted != (agentConfigOmitted{}) {
		entry.Excluded = &omitted
	}

	return entry
}

// agentConfigOmittedFrom records which excluded fields the source agent
// actually had. mcp_config counts as present when it is either returned or
// redacted — a non-owner export must still warn that the agent has one.
func agentConfigOmittedFrom(src map[string]any) agentConfigOmitted {
	omitted := agentConfigOmitted{}

	if hasEnv, ok := src["has_custom_env"].(bool); ok && hasEnv {
		omitted.HasCustomEnv = true
	}
	if count, ok := src["custom_env_key_count"].(float64); ok && count > 0 && count == math.Trunc(count) {
		omitted.CustomEnvKeyCount = int(count)
		omitted.HasCustomEnv = true
	}

	if redacted, ok := src["mcp_config_redacted"].(bool); ok && redacted {
		omitted.HasMcpConfig = true
	}
	if mcp, ok := src["mcp_config"]; ok && mcp != nil {
		omitted.HasMcpConfig = true
	}

	if rc, ok := src["runtime_config"].(map[string]any); ok && len(rc) > 0 {
		omitted.HasRuntimeConfig = true
	}

	return omitted
}

// decodeAgentConfigDoc parses and validates a document read from disk or
// stdin. Validation is deliberately strict about the envelope — a wrong kind
// or a future version is an error, not a best-effort import that half-applies
// a format this binary does not understand.
func decodeAgentConfigDoc(raw []byte) (*agentConfigDoc, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, fmt.Errorf("empty document")
	}

	var doc agentConfigDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("not a valid agent configuration document: %w", err)
	}
	if doc.Kind != agentConfigDocKind {
		return nil, fmt.Errorf("unexpected document kind %q, want %q (was this produced by 'multica agent export'?)", doc.Kind, agentConfigDocKind)
	}
	if doc.Version != agentConfigDocVersion {
		return nil, fmt.Errorf("unsupported document version %d, this CLI reads version %d; upgrade with 'multica update'", doc.Version, agentConfigDocVersion)
	}
	if len(doc.Agents) == 0 {
		return nil, fmt.Errorf("document contains no agents")
	}
	// Names are how an import matches an entry against the target workspace, so
	// two entries sharing one is ambiguous no matter which conflict strategy is
	// in play — reject it here rather than letting the second entry look like a
	// collision with the first one's freshly created agent.
	seen := map[string]bool{}
	for i := range doc.Agents {
		doc.Agents[i].Name = strings.TrimSpace(doc.Agents[i].Name)
		name := doc.Agents[i].Name
		if name == "" {
			return nil, fmt.Errorf("agents[%d] has no name", i)
		}
		if seen[name] {
			return nil, fmt.Errorf("document contains more than one agent named %q; names must be unique within a document", name)
		}
		seen[name] = true
	}

	return &doc, nil
}
