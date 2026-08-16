package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func newAgentImportTestCmd(t *testing.T) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	c := &cobra.Command{Use: "import"}
	registerAgentImportFlags(c)
	c.Flags().String("profile", "", "")
	out := &bytes.Buffer{}
	c.SetOut(out)
	return c, out
}

// importDoc builds a one-agent document exported from workspace ws-1.
func importDoc(mutate ...func(*agentConfigDoc)) agentConfigDoc {
	max := int32(9)
	doc := agentConfigDoc{
		Kind:    agentConfigDocKind,
		Version: agentConfigDocVersion,
		Source:  agentConfigSource{WorkspaceID: "ws-1", ExportedAt: "2026-08-16T00:00:00Z"},
		Agents: []agentConfigEntry{{
			Name:               "Src",
			Description:        "a description",
			Instructions:       "some instructions",
			AvatarURL:          "emoji:X",
			Model:              "claude-sonnet-4-6",
			ThinkingLevel:      "high",
			ServiceTier:        "priority",
			CustomArgs:         []string{"--foo"},
			MaxConcurrentTasks: &max,
			PermissionMode:     "public_to",
			InvocationTargets: []agentConfigTarget{
				{TargetType: "workspace"},
				{TargetType: "member", TargetID: "user-7"},
			},
			Skills:   []agentConfigSkill{{ID: "skill-1", Name: "One"}},
			Origin:   &agentConfigOrigin{AgentID: "agent-src", RuntimeID: "runtime-1", RuntimeMode: "local"},
			Excluded: &agentConfigOmitted{HasCustomEnv: true, CustomEnvKeyCount: 2, HasMcpConfig: true},
		}},
	}
	for _, m := range mutate {
		m(&doc)
	}
	return doc
}

func writeImportDoc(t *testing.T, doc agentConfigDoc) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agents.json")
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal document: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write document: %v", err)
	}
	return path
}

// importCalls records every mutating request the import made, in order.
type importCalls struct {
	posts   []map[string]any
	puts    map[string]map[string]any
	putSeq  []string
	skills  map[string][]string
	envSets map[string]map[string]any
}

// importMockServer serves the workspace catalog and captures writes.
func importMockServer(t *testing.T, existing []map[string]any, skills []map[string]any, calls *importCalls) *httptest.Server {
	t.Helper()
	calls.puts = map[string]map[string]any{}
	calls.skills = map[string][]string{}
	calls.envSets = map[string]map[string]any{}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && path == "/api/agents":
			_ = json.NewEncoder(w).Encode(existing)
		case r.Method == http.MethodGet && path == "/api/skills":
			_ = json.NewEncoder(w).Encode(skills)
		case r.Method == http.MethodPost && path == "/api/agents":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode create body: %v", err)
			}
			calls.posts = append(calls.posts, body)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "agent-new", "name": body["name"]})
		case r.Method == http.MethodPut && strings.HasSuffix(path, "/skills"):
			var body struct {
				SkillIDs []string `json:"skill_ids"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode skills body: %v", err)
			}
			calls.skills[strings.TrimSuffix(strings.TrimPrefix(path, "/api/agents/"), "/skills")] = body.SkillIDs
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == http.MethodPut && strings.HasSuffix(path, "/env"):
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode env body: %v", err)
			}
			calls.envSets[strings.TrimSuffix(strings.TrimPrefix(path, "/api/agents/"), "/env")] = body
			_ = json.NewEncoder(w).Encode(map[string]any{"custom_env": body["custom_env"]})
		case r.Method == http.MethodPut && strings.HasPrefix(path, "/api/agents/"):
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode update body: %v", err)
			}
			id := strings.TrimPrefix(path, "/api/agents/")
			calls.puts[id] = body
			calls.putSeq = append(calls.putSeq, id)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "name": body["name"]})
		default:
			t.Errorf("unexpected request %s %s", r.Method, path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestAgentImportCreatesFromDocument(t *testing.T) {
	calls := &importCalls{}
	srv := importMockServer(t, nil, []map[string]any{{"id": "skill-1", "name": "One"}}, calls)
	defer srv.Close()
	setCopyTestEnv(t, srv.URL)

	cmd, _ := newAgentImportTestCmd(t)
	_ = cmd.Flags().Set("file", writeImportDoc(t, importDoc()))

	if err := runAgentImport(cmd, nil); err != nil {
		t.Fatalf("runAgentImport: %v", err)
	}
	if len(calls.posts) != 1 {
		t.Fatalf("posts = %d, want 1", len(calls.posts))
	}

	body := calls.posts[0]
	if body["name"] != "Src" || body["description"] != "a description" || body["instructions"] != "some instructions" {
		t.Errorf("text fields = %v", body)
	}
	// Same workspace, no --runtime-id: the agent returns to its recorded runtime
	// and keeps its runtime-specific tuning.
	if body["runtime_id"] != "runtime-1" {
		t.Errorf("runtime_id = %v, want runtime-1", body["runtime_id"])
	}
	if body["model"] != "claude-sonnet-4-6" || body["thinking_level"] != "high" || body["service_tier"] != "priority" {
		t.Errorf("runtime tuning = %v", body)
	}
	if body["max_concurrent_tasks"] != float64(9) {
		t.Errorf("max_concurrent_tasks = %v", body["max_concurrent_tasks"])
	}
	if !reflect.DeepEqual(body["custom_args"], []any{"--foo"}) {
		t.Errorf("custom_args = %v", body["custom_args"])
	}
	// Same workspace: the member allow-list entry is still meaningful.
	if body["permission_mode"] != "public_to" {
		t.Errorf("permission_mode = %v", body["permission_mode"])
	}
	if got, _ := body["invocation_targets"].([]any); len(got) != 2 {
		t.Errorf("invocation_targets = %v, want both entries", body["invocation_targets"])
	}
	// Skills bind in the same create transaction.
	if !reflect.DeepEqual(body["skill_ids"], []any{"skill-1"}) {
		t.Errorf("skill_ids = %v", body["skill_ids"])
	}
}

// A document is not a credential store: nothing in it can produce env, mcp or
// runtime config on the new agent unless the operator passes it explicitly.
func TestAgentImportNeverInventsSecrets(t *testing.T) {
	calls := &importCalls{}
	srv := importMockServer(t, nil, []map[string]any{{"id": "skill-1", "name": "One"}}, calls)
	defer srv.Close()
	setCopyTestEnv(t, srv.URL)

	cmd, _ := newAgentImportTestCmd(t)
	_ = cmd.Flags().Set("file", writeImportDoc(t, importDoc()))

	if err := runAgentImport(cmd, nil); err != nil {
		t.Fatalf("runAgentImport: %v", err)
	}
	for _, key := range []string{"custom_env", "mcp_config", "runtime_config"} {
		if _, ok := calls.posts[0][key]; ok {
			t.Errorf("create body must not contain %q, got %v", key, calls.posts[0][key])
		}
	}
	if len(calls.envSets) != 0 {
		t.Errorf("env endpoint must not be called, got %v", calls.envSets)
	}
}

// The excluded-field record is what turns "the import worked" into "the import
// worked but this agent still needs its API keys".
func TestAgentImportWarnsAboutExcludedSecrets(t *testing.T) {
	calls := &importCalls{}
	srv := importMockServer(t, nil, []map[string]any{{"id": "skill-1", "name": "One"}}, calls)
	defer srv.Close()
	setCopyTestEnv(t, srv.URL)

	cmd, out := newAgentImportTestCmd(t)
	_ = cmd.Flags().Set("file", writeImportDoc(t, importDoc()))

	if err := runAgentImport(cmd, nil); err != nil {
		t.Fatalf("runAgentImport: %v", err)
	}
	report := out.String()
	if !strings.Contains(report, "custom_env") || !strings.Contains(report, "mcp_config") {
		t.Errorf("report must name the omitted secrets:\n%s", report)
	}
}

func TestAgentImportExplicitSecretsRideAlongOnCreate(t *testing.T) {
	calls := &importCalls{}
	srv := importMockServer(t, nil, []map[string]any{{"id": "skill-1", "name": "One"}}, calls)
	defer srv.Close()
	setCopyTestEnv(t, srv.URL)

	cmd, _ := newAgentImportTestCmd(t)
	_ = cmd.Flags().Set("file", writeImportDoc(t, importDoc()))
	_ = cmd.Flags().Set("custom-env", `{"API_KEY":"fresh"}`)
	_ = cmd.Flags().Set("mcp-config", `{"mcpServers":{}}`)

	if err := runAgentImport(cmd, nil); err != nil {
		t.Fatalf("runAgentImport: %v", err)
	}
	env, ok := calls.posts[0]["custom_env"].(map[string]any)
	if !ok || env["API_KEY"] != "fresh" {
		t.Errorf("custom_env = %v", calls.posts[0]["custom_env"])
	}
	if _, ok := calls.posts[0]["mcp_config"]; !ok {
		t.Error("mcp_config must be sent when supplied explicitly")
	}
}

func TestAgentImportRejectsPerAgentFlagsOnMultiAgentDocument(t *testing.T) {
	doc := importDoc(func(d *agentConfigDoc) {
		second := d.Agents[0]
		second.Name = "Other"
		d.Agents = append(d.Agents, second)
	})
	path := writeImportDoc(t, doc)

	for _, flag := range []string{"name", "custom-env", "runtime-config"} {
		t.Run(flag, func(t *testing.T) {
			setCopyTestEnv(t, "http://127.0.0.1:1")
			cmd, _ := newAgentImportTestCmd(t)
			_ = cmd.Flags().Set("file", path)
			_ = cmd.Flags().Set(flag, `{}`)

			err := runAgentImport(cmd, nil)
			if err == nil || !strings.Contains(err.Error(), "single agent") {
				t.Fatalf("error = %v, want a single-agent refusal", err)
			}
		})
	}
}

// fail is the default: a collision aborts the run before anything is written.
func TestAgentImportConflictFailWritesNothing(t *testing.T) {
	calls := &importCalls{}
	existing := []map[string]any{{"id": "agent-existing", "name": "Src"}}
	srv := importMockServer(t, existing, []map[string]any{{"id": "skill-1", "name": "One"}}, calls)
	defer srv.Close()
	setCopyTestEnv(t, srv.URL)

	cmd, _ := newAgentImportTestCmd(t)
	_ = cmd.Flags().Set("file", writeImportDoc(t, importDoc()))

	err := runAgentImport(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "Src") {
		t.Fatalf("error = %v, want a conflict naming Src", err)
	}
	if len(calls.posts) != 0 || len(calls.puts) != 0 {
		t.Errorf("nothing may be written on a failed conflict check: posts=%v puts=%v", calls.posts, calls.puts)
	}
}

func TestAgentImportConflictSkipLeavesExistingAlone(t *testing.T) {
	calls := &importCalls{}
	existing := []map[string]any{{"id": "agent-existing", "name": "Src"}}
	srv := importMockServer(t, existing, []map[string]any{{"id": "skill-1", "name": "One"}}, calls)
	defer srv.Close()
	setCopyTestEnv(t, srv.URL)

	cmd, out := newAgentImportTestCmd(t)
	_ = cmd.Flags().Set("file", writeImportDoc(t, importDoc()))
	_ = cmd.Flags().Set("on-conflict", "skip")

	if err := runAgentImport(cmd, nil); err != nil {
		t.Fatalf("runAgentImport: %v", err)
	}
	if len(calls.posts) != 0 || len(calls.puts) != 0 {
		t.Errorf("skip must not write: posts=%v puts=%v", calls.posts, calls.puts)
	}
	if !strings.Contains(out.String(), "skip") {
		t.Errorf("report must record the skip:\n%s", out.String())
	}
}

func TestAgentImportConflictRenameCreatesSuffixedAgent(t *testing.T) {
	calls := &importCalls{}
	existing := []map[string]any{
		{"id": "agent-existing", "name": "Src"},
		{"id": "agent-existing-2", "name": "Src (2)"},
	}
	srv := importMockServer(t, existing, []map[string]any{{"id": "skill-1", "name": "One"}}, calls)
	defer srv.Close()
	setCopyTestEnv(t, srv.URL)

	cmd, _ := newAgentImportTestCmd(t)
	_ = cmd.Flags().Set("file", writeImportDoc(t, importDoc()))
	_ = cmd.Flags().Set("on-conflict", "rename")

	if err := runAgentImport(cmd, nil); err != nil {
		t.Fatalf("runAgentImport: %v", err)
	}
	if len(calls.posts) != 1 {
		t.Fatalf("posts = %d, want 1", len(calls.posts))
	}
	if calls.posts[0]["name"] != "Src (3)" {
		t.Errorf("name = %v, want \"Src (3)\" (Src and Src (2) are taken)", calls.posts[0]["name"])
	}
}

func TestAgentImportConflictOverwriteUpdatesInPlace(t *testing.T) {
	calls := &importCalls{}
	existing := []map[string]any{{"id": "agent-existing", "name": "Src"}}
	srv := importMockServer(t, existing, []map[string]any{{"id": "skill-1", "name": "One"}}, calls)
	defer srv.Close()
	setCopyTestEnv(t, srv.URL)

	cmd, _ := newAgentImportTestCmd(t)
	_ = cmd.Flags().Set("file", writeImportDoc(t, importDoc()))
	_ = cmd.Flags().Set("on-conflict", "overwrite")

	if err := runAgentImport(cmd, nil); err != nil {
		t.Fatalf("runAgentImport: %v", err)
	}
	if len(calls.posts) != 0 {
		t.Errorf("overwrite must not create: %v", calls.posts)
	}
	body, ok := calls.puts["agent-existing"]
	if !ok {
		t.Fatalf("expected an update on agent-existing, got %v", calls.puts)
	}
	if body["instructions"] != "some instructions" || body["model"] != "claude-sonnet-4-6" {
		t.Errorf("update body = %v", body)
	}
	// Bindings are replaced through the dedicated skills endpoint, since the
	// generic update endpoint does not accept skill_ids.
	if !reflect.DeepEqual(calls.skills["agent-existing"], []string{"skill-1"}) {
		t.Errorf("skills = %v", calls.skills["agent-existing"])
	}
}

// An ambiguous name cannot pick itself a winner.
func TestAgentImportOverwriteRefusesAmbiguousName(t *testing.T) {
	calls := &importCalls{}
	existing := []map[string]any{
		{"id": "agent-a", "name": "Src"},
		{"id": "agent-b", "name": "Src"},
	}
	srv := importMockServer(t, existing, []map[string]any{{"id": "skill-1", "name": "One"}}, calls)
	defer srv.Close()
	setCopyTestEnv(t, srv.URL)

	cmd, _ := newAgentImportTestCmd(t)
	_ = cmd.Flags().Set("file", writeImportDoc(t, importDoc()))
	_ = cmd.Flags().Set("on-conflict", "overwrite")

	err := runAgentImport(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "--into") {
		t.Fatalf("error = %v, want it to point at --into", err)
	}
	if len(calls.puts) != 0 {
		t.Errorf("nothing may be written: %v", calls.puts)
	}
}

func TestAgentImportIntoTargetsSpecificAgent(t *testing.T) {
	calls := &importCalls{}
	existing := []map[string]any{{"id": "agent-other", "name": "Unrelated"}}
	srv := importMockServer(t, existing, []map[string]any{{"id": "skill-1", "name": "One"}}, calls)
	defer srv.Close()
	setCopyTestEnv(t, srv.URL)

	cmd, _ := newAgentImportTestCmd(t)
	_ = cmd.Flags().Set("file", writeImportDoc(t, importDoc()))
	_ = cmd.Flags().Set("into", "agent-other")

	if err := runAgentImport(cmd, nil); err != nil {
		t.Fatalf("runAgentImport: %v", err)
	}
	if _, ok := calls.puts["agent-other"]; !ok {
		t.Fatalf("expected an update on agent-other, got %v", calls.puts)
	}
	if len(calls.posts) != 0 {
		t.Errorf("--into must not create: %v", calls.posts)
	}
}

func TestAgentImportIntoRejectsOnConflict(t *testing.T) {
	setCopyTestEnv(t, "http://127.0.0.1:1")
	cmd, _ := newAgentImportTestCmd(t)
	_ = cmd.Flags().Set("file", writeImportDoc(t, importDoc()))
	_ = cmd.Flags().Set("into", "agent-other")
	_ = cmd.Flags().Set("on-conflict", "overwrite")

	err := runAgentImport(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "--on-conflict") {
		t.Fatalf("error = %v, want a mutual-exclusion refusal", err)
	}
}

// A cross-workspace import cannot reuse the source's runtime id, and the model
// chosen for that runtime may not exist on the new one.
func TestAgentImportCrossWorkspaceRequiresRuntimeID(t *testing.T) {
	calls := &importCalls{}
	srv := importMockServer(t, nil, []map[string]any{{"id": "skill-x", "name": "One"}}, calls)
	defer srv.Close()
	setCopyTestEnv(t, srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-2")

	cmd, _ := newAgentImportTestCmd(t)
	_ = cmd.Flags().Set("file", writeImportDoc(t, importDoc()))

	err := runAgentImport(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "--runtime-id") {
		t.Fatalf("error = %v, want it to demand --runtime-id", err)
	}
	if len(calls.posts) != 0 {
		t.Errorf("nothing may be written: %v", calls.posts)
	}
}

func TestAgentImportDifferentRuntimeRequiresModel(t *testing.T) {
	calls := &importCalls{}
	srv := importMockServer(t, nil, []map[string]any{{"id": "skill-1", "name": "One"}}, calls)
	defer srv.Close()
	setCopyTestEnv(t, srv.URL)

	cmd, _ := newAgentImportTestCmd(t)
	_ = cmd.Flags().Set("file", writeImportDoc(t, importDoc()))
	_ = cmd.Flags().Set("runtime-id", "runtime-2")

	err := runAgentImport(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "--model") {
		t.Fatalf("error = %v, want it to demand --model", err)
	}
	if len(calls.posts) != 0 {
		t.Errorf("nothing may be written: %v", calls.posts)
	}
}

func TestAgentImportDifferentRuntimeDropsRuntimeSpecificFields(t *testing.T) {
	calls := &importCalls{}
	srv := importMockServer(t, nil, []map[string]any{{"id": "skill-1", "name": "One"}}, calls)
	defer srv.Close()
	setCopyTestEnv(t, srv.URL)

	cmd, _ := newAgentImportTestCmd(t)
	_ = cmd.Flags().Set("file", writeImportDoc(t, importDoc()))
	_ = cmd.Flags().Set("runtime-id", "runtime-2")
	_ = cmd.Flags().Set("model", "openai/gpt-4o")

	if err := runAgentImport(cmd, nil); err != nil {
		t.Fatalf("runAgentImport: %v", err)
	}
	body := calls.posts[0]
	if body["runtime_id"] != "runtime-2" || body["model"] != "openai/gpt-4o" {
		t.Errorf("runtime/model = %v / %v", body["runtime_id"], body["model"])
	}
	for _, key := range []string{"thinking_level", "service_tier"} {
		if _, ok := body[key]; ok {
			t.Errorf("%s must not ride across a runtime change, got %v", key, body[key])
		}
	}
}

// Member allow-list entries name ids from the source workspace; carrying them
// over would grant invocation rights to whoever holds that id elsewhere.
func TestAgentImportCrossWorkspaceDropsMemberTargets(t *testing.T) {
	calls := &importCalls{}
	srv := importMockServer(t, nil, []map[string]any{{"id": "skill-x", "name": "One"}}, calls)
	defer srv.Close()
	setCopyTestEnv(t, srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-2")

	cmd, _ := newAgentImportTestCmd(t)
	_ = cmd.Flags().Set("file", writeImportDoc(t, importDoc()))
	_ = cmd.Flags().Set("runtime-id", "runtime-9")
	_ = cmd.Flags().Set("model", "")

	if err := runAgentImport(cmd, nil); err != nil {
		t.Fatalf("runAgentImport: %v", err)
	}
	targets, _ := calls.posts[0]["invocation_targets"].([]any)
	if len(targets) != 1 {
		t.Fatalf("invocation_targets = %v, want only the workspace entry", calls.posts[0]["invocation_targets"])
	}
	target, _ := targets[0].(map[string]any)
	if target["target_type"] != "workspace" {
		t.Errorf("surviving target = %v", target)
	}
	if _, ok := target["target_id"]; ok {
		t.Errorf("workspace target must not carry a source id, got %v", target)
	}
}

// With nothing left in the allow-list, public_to would mean "invokable by
// nobody" — deny-by-default private is the honest result.
func TestAgentImportCrossWorkspaceMemberOnlyAllowlistFallsBackToPrivate(t *testing.T) {
	calls := &importCalls{}
	srv := importMockServer(t, nil, []map[string]any{{"id": "skill-x", "name": "One"}}, calls)
	defer srv.Close()
	setCopyTestEnv(t, srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-2")

	doc := importDoc(func(d *agentConfigDoc) {
		d.Agents[0].InvocationTargets = []agentConfigTarget{{TargetType: "member", TargetID: "user-7"}}
	})

	cmd, out := newAgentImportTestCmd(t)
	_ = cmd.Flags().Set("file", writeImportDoc(t, doc))
	_ = cmd.Flags().Set("runtime-id", "runtime-9")
	_ = cmd.Flags().Set("model", "")

	if err := runAgentImport(cmd, nil); err != nil {
		t.Fatalf("runAgentImport: %v", err)
	}
	if calls.posts[0]["permission_mode"] != "private" {
		t.Errorf("permission_mode = %v, want private", calls.posts[0]["permission_mode"])
	}
	if !strings.Contains(out.String(), "private") {
		t.Errorf("report must explain the downgrade:\n%s", out.String())
	}
}

// Skill ids do not survive a workspace change; a unique name match does.
func TestAgentImportResolvesSkillsByNameAcrossWorkspaces(t *testing.T) {
	calls := &importCalls{}
	srv := importMockServer(t, nil, []map[string]any{{"id": "skill-elsewhere", "name": "One"}}, calls)
	defer srv.Close()
	setCopyTestEnv(t, srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-2")

	cmd, _ := newAgentImportTestCmd(t)
	_ = cmd.Flags().Set("file", writeImportDoc(t, importDoc()))
	_ = cmd.Flags().Set("runtime-id", "runtime-9")
	_ = cmd.Flags().Set("model", "")

	if err := runAgentImport(cmd, nil); err != nil {
		t.Fatalf("runAgentImport: %v", err)
	}
	if !reflect.DeepEqual(calls.posts[0]["skill_ids"], []any{"skill-elsewhere"}) {
		t.Errorf("skill_ids = %v, want the name-matched id", calls.posts[0]["skill_ids"])
	}
}

func TestAgentImportReportsUnresolvableSkills(t *testing.T) {
	calls := &importCalls{}
	srv := importMockServer(t, nil, []map[string]any{{"id": "skill-other", "name": "Something Else"}}, calls)
	defer srv.Close()
	setCopyTestEnv(t, srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-2")

	cmd, out := newAgentImportTestCmd(t)
	_ = cmd.Flags().Set("file", writeImportDoc(t, importDoc()))
	_ = cmd.Flags().Set("runtime-id", "runtime-9")
	_ = cmd.Flags().Set("model", "")

	if err := runAgentImport(cmd, nil); err != nil {
		t.Fatalf("runAgentImport: %v", err)
	}
	if _, ok := calls.posts[0]["skill_ids"]; ok {
		t.Errorf("unresolved skills must not be sent, got %v", calls.posts[0]["skill_ids"])
	}
	if !strings.Contains(out.String(), "does not exist in this workspace") {
		t.Errorf("report must name the missing skill:\n%s", out.String())
	}
}

func TestAgentImportNoSkillsLeavesBindingsAlone(t *testing.T) {
	calls := &importCalls{}
	existing := []map[string]any{{"id": "agent-existing", "name": "Src"}}
	srv := importMockServer(t, existing, nil, calls)
	defer srv.Close()
	setCopyTestEnv(t, srv.URL)

	cmd, _ := newAgentImportTestCmd(t)
	_ = cmd.Flags().Set("file", writeImportDoc(t, importDoc()))
	_ = cmd.Flags().Set("on-conflict", "overwrite")
	_ = cmd.Flags().Set("no-skills", "true")

	if err := runAgentImport(cmd, nil); err != nil {
		t.Fatalf("runAgentImport: %v", err)
	}
	if len(calls.skills) != 0 {
		t.Errorf("--no-skills must not touch bindings, got %v", calls.skills)
	}
}

func TestAgentImportDryRunWritesNothing(t *testing.T) {
	calls := &importCalls{}
	existing := []map[string]any{{"id": "agent-existing", "name": "Src"}}
	srv := importMockServer(t, existing, []map[string]any{{"id": "skill-1", "name": "One"}}, calls)
	defer srv.Close()
	setCopyTestEnv(t, srv.URL)

	cmd, out := newAgentImportTestCmd(t)
	_ = cmd.Flags().Set("file", writeImportDoc(t, importDoc()))
	_ = cmd.Flags().Set("on-conflict", "overwrite")
	_ = cmd.Flags().Set("dry-run", "true")

	if err := runAgentImport(cmd, nil); err != nil {
		t.Fatalf("runAgentImport: %v", err)
	}
	if len(calls.posts) != 0 || len(calls.puts) != 0 || len(calls.skills) != 0 {
		t.Errorf("dry run wrote something: posts=%v puts=%v skills=%v", calls.posts, calls.puts, calls.skills)
	}

	var report struct {
		DryRun bool              `json:"dry_run"`
		Agents []agentImportPlan `json:"agents"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, out.String())
	}
	if !report.DryRun || len(report.Agents) != 1 {
		t.Fatalf("report = %+v", report)
	}
	if report.Agents[0].Action != "overwrite" || report.Agents[0].AgentID != "agent-existing" {
		t.Errorf("plan = %+v", report.Agents[0])
	}
}

// The envelope is validated strictly: half-applying a format this binary does
// not understand is worse than refusing.
func TestAgentImportRejectsForeignDocuments(t *testing.T) {
	cases := map[string]agentConfigDoc{
		"wrong kind":    {Kind: "something.else", Version: 1, Agents: []agentConfigEntry{{Name: "A"}}},
		"future format": {Kind: agentConfigDocKind, Version: agentConfigDocVersion + 1, Agents: []agentConfigEntry{{Name: "A"}}},
		"no agents":     {Kind: agentConfigDocKind, Version: agentConfigDocVersion},
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			setCopyTestEnv(t, "http://127.0.0.1:1")
			cmd, _ := newAgentImportTestCmd(t)
			_ = cmd.Flags().Set("file", writeImportDoc(t, doc))
			if err := runAgentImport(cmd, nil); err == nil {
				t.Fatal("expected the document to be rejected")
			}
		})
	}
}

func TestAgentImportRejectsDuplicateNamesInDocument(t *testing.T) {
	doc := importDoc(func(d *agentConfigDoc) {
		d.Agents = append(d.Agents, d.Agents[0])
	})
	setCopyTestEnv(t, "http://127.0.0.1:1")

	cmd, _ := newAgentImportTestCmd(t)
	_ = cmd.Flags().Set("file", writeImportDoc(t, doc))

	err := runAgentImport(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "unique") {
		t.Fatalf("error = %v, want a duplicate-name refusal", err)
	}
}

func TestAgentImportRequiresADocumentSource(t *testing.T) {
	setCopyTestEnv(t, "http://127.0.0.1:1")
	cmd, _ := newAgentImportTestCmd(t)
	err := runAgentImport(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "--file") {
		t.Fatalf("error = %v, want it to mention --file", err)
	}
}

func TestAgentImportRejectsCompetingStdinReaders(t *testing.T) {
	setCopyTestEnv(t, "http://127.0.0.1:1")
	cmd, _ := newAgentImportTestCmd(t)
	_ = cmd.Flags().Set("stdin", "true")
	_ = cmd.Flags().Set("custom-env-stdin", "true")

	err := runAgentImport(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "stdin") {
		t.Fatalf("error = %v, want a stdin conflict refusal", err)
	}
}

func TestAgentImportRejectsUnknownConflictStrategy(t *testing.T) {
	cmd, _ := newAgentImportTestCmd(t)
	_ = cmd.Flags().Set("on-conflict", "merge")
	err := runAgentImport(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "fail, overwrite, rename, skip") {
		t.Fatalf("error = %v, want the valid-strategy list", err)
	}
}

// Round-tripping is the contract users will actually rely on: export, import,
// and the created agent matches what the source agent exposed.
func TestAgentExportImportRoundTrip(t *testing.T) {
	exportSrv := exportMockServer(t, exportableAgent())
	defer exportSrv.Close()
	setCopyTestEnv(t, exportSrv.URL)

	path := filepath.Join(t.TempDir(), "agents.json")
	exportCmd, _ := newAgentExportTestCmd(t)
	_ = exportCmd.Flags().Set("file", path)
	if err := runAgentExport(exportCmd, []string{"agent-src"}); err != nil {
		t.Fatalf("runAgentExport: %v", err)
	}

	calls := &importCalls{}
	importSrv := importMockServer(t, nil, []map[string]any{{"id": "skill-1", "name": "One"}, {"id": "skill-2", "name": "Two"}}, calls)
	defer importSrv.Close()
	setCopyTestEnv(t, importSrv.URL)

	importCmd, _ := newAgentImportTestCmd(t)
	_ = importCmd.Flags().Set("file", path)
	if err := runAgentImport(importCmd, nil); err != nil {
		t.Fatalf("runAgentImport: %v", err)
	}

	body := calls.posts[0]
	source := exportableAgent()
	for _, key := range []string{"name", "description", "instructions", "avatar_url", "model", "thinking_level", "service_tier", "permission_mode"} {
		if body[key] != source[key] {
			t.Errorf("%s = %v, want %v", key, body[key], source[key])
		}
	}
	if body["runtime_id"] != "runtime-1" {
		t.Errorf("runtime_id = %v", body["runtime_id"])
	}
	if !reflect.DeepEqual(body["skill_ids"], []any{"skill-1", "skill-2"}) {
		t.Errorf("skill_ids = %v", body["skill_ids"])
	}
}
