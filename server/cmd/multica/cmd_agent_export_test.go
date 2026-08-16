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

// newAgentExportTestCmd builds a standalone cobra.Command carrying the flags
// runAgentExport reads (via the shared registrar), plus the persistent
// --profile flag the API-client resolver needs.
func newAgentExportTestCmd(t *testing.T) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	c := &cobra.Command{Use: "export"}
	registerAgentExportFlags(c)
	c.Flags().String("profile", "", "")
	out := &bytes.Buffer{}
	c.SetOut(out)
	return c, out
}

// exportableAgent is a representative GET /api/agents/<id> response carrying
// every portable field plus the secret metadata an export must never turn into
// real values.
func exportableAgent() map[string]any {
	return map[string]any{
		"id":                   "agent-src",
		"name":                 "Src",
		"runtime_id":           "runtime-1",
		"runtime_mode":         "local",
		"description":          "a description",
		"instructions":         "some instructions",
		"avatar_url":           "emoji:X",
		"custom_args":          []any{"--foo", "--bar"},
		"max_concurrent_tasks": 9,
		"model":                "claude-sonnet-4-6",
		"thinking_level":       "high",
		"service_tier":         "priority",
		"permission_mode":      "public_to",
		"invocation_targets": []any{
			map[string]any{"target_type": "workspace"},
			map[string]any{"target_type": "member", "target_id": "user-7"},
		},
		"skills": []any{
			map[string]any{"id": "skill-1", "name": "One"},
			map[string]any{"id": "skill-2", "name": "Two"},
		},
		"has_custom_env":       true,
		"custom_env_key_count": float64(2),
		"mcp_config_redacted":  true,
		"runtime_config":       map[string]any{"gateway": map[string]any{"token": "***"}},
	}
}

func exportMockServer(t *testing.T, agents ...map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Path == "/api/agents" {
			_ = json.NewEncoder(w).Encode(agents)
			return
		}
		for _, a := range agents {
			if r.URL.Path == "/api/agents/"+a["id"].(string) {
				_ = json.NewEncoder(w).Encode(a)
				return
			}
		}
		t.Errorf("unexpected GET %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
}

func TestAgentExportWritesPortableFields(t *testing.T) {
	srv := exportMockServer(t, exportableAgent())
	defer srv.Close()
	setCopyTestEnv(t, srv.URL)

	cmd, out := newAgentExportTestCmd(t)
	if err := runAgentExport(cmd, []string{"agent-src"}); err != nil {
		t.Fatalf("runAgentExport: %v", err)
	}

	var doc agentConfigDoc
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("decode exported document: %v\n%s", err, out.String())
	}
	if doc.Kind != agentConfigDocKind || doc.Version != agentConfigDocVersion {
		t.Fatalf("envelope = %q v%d", doc.Kind, doc.Version)
	}
	if doc.Source.WorkspaceID != "ws-1" {
		t.Errorf("source.workspace_id = %q, want ws-1", doc.Source.WorkspaceID)
	}
	if len(doc.Agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(doc.Agents))
	}

	entry := doc.Agents[0]
	if entry.Name != "Src" || entry.Description != "a description" || entry.Instructions != "some instructions" {
		t.Errorf("text fields = %+v", entry)
	}
	if entry.AvatarURL != "emoji:X" {
		t.Errorf("avatar_url = %q", entry.AvatarURL)
	}
	if entry.Model != "claude-sonnet-4-6" || entry.ThinkingLevel != "high" || entry.ServiceTier != "priority" {
		t.Errorf("runtime tuning = %+v", entry)
	}
	if !reflect.DeepEqual(entry.CustomArgs, []string{"--foo", "--bar"}) {
		t.Errorf("custom_args = %v", entry.CustomArgs)
	}
	if entry.MaxConcurrentTasks == nil || *entry.MaxConcurrentTasks != 9 {
		t.Errorf("max_concurrent_tasks = %v", entry.MaxConcurrentTasks)
	}
	if entry.PermissionMode != "public_to" || len(entry.InvocationTargets) != 2 {
		t.Errorf("permission = %q %v", entry.PermissionMode, entry.InvocationTargets)
	}
	if !reflect.DeepEqual(entry.Skills, []agentConfigSkill{{ID: "skill-1", Name: "One"}, {ID: "skill-2", Name: "Two"}}) {
		t.Errorf("skills = %v", entry.Skills)
	}
	if entry.Origin == nil || entry.Origin.RuntimeID != "runtime-1" || entry.Origin.AgentID != "agent-src" {
		t.Errorf("origin = %+v", entry.Origin)
	}
}

// The whole point of the document is that it can be handed around, so it must
// never contain the secret / machine-local fields — not even the masked or
// redacted placeholders the API returns.
func TestAgentExportNeverWritesSecrets(t *testing.T) {
	srv := exportMockServer(t, exportableAgent())
	defer srv.Close()
	setCopyTestEnv(t, srv.URL)

	cmd, out := newAgentExportTestCmd(t)
	if err := runAgentExport(cmd, []string{"agent-src"}); err != nil {
		t.Fatalf("runAgentExport: %v", err)
	}

	var generic map[string]any
	if err := json.Unmarshal(out.Bytes(), &generic); err != nil {
		t.Fatalf("decode: %v", err)
	}
	agents, _ := generic["agents"].([]any)
	if len(agents) != 1 {
		t.Fatalf("agents = %v", generic["agents"])
	}
	entry, _ := agents[0].(map[string]any)
	for _, key := range []string{"custom_env", "mcp_config", "runtime_config"} {
		if _, ok := entry[key]; ok {
			t.Errorf("document must not contain %q, got %v", key, entry[key])
		}
	}
	// The gateway token mask must not leak through any field either.
	if strings.Contains(out.String(), "gateway") {
		t.Errorf("document leaked runtime_config content:\n%s", out.String())
	}

	// Presence is recorded so an import can warn about what still needs setting.
	excluded, _ := entry["excluded"].(map[string]any)
	if excluded["has_custom_env"] != true || excluded["custom_env_key_count"] != float64(2) {
		t.Errorf("excluded.custom_env = %v", excluded)
	}
	if excluded["has_mcp_config"] != true || excluded["has_runtime_config"] != true {
		t.Errorf("excluded = %v", excluded)
	}
}

func TestAgentExportAllSkipsArchivedAgents(t *testing.T) {
	active := exportableAgent()
	archived := exportableAgent()
	archived["id"] = "agent-archived"
	archived["name"] = "Archived"
	archived["archived_at"] = "2026-01-01T00:00:00Z"

	srv := exportMockServer(t, active, archived)
	defer srv.Close()
	setCopyTestEnv(t, srv.URL)

	cmd, out := newAgentExportTestCmd(t)
	_ = cmd.Flags().Set("all", "true")
	if err := runAgentExport(cmd, nil); err != nil {
		t.Fatalf("runAgentExport: %v", err)
	}

	var doc agentConfigDoc
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(doc.Agents) != 1 || doc.Agents[0].Name != "Src" {
		t.Fatalf("agents = %+v, want only the active one", doc.Agents)
	}
}

func TestAgentExportRejectsIdsWithAll(t *testing.T) {
	cmd, _ := newAgentExportTestCmd(t)
	_ = cmd.Flags().Set("all", "true")
	if err := runAgentExport(cmd, []string{"agent-src"}); err == nil {
		t.Fatal("expected an error when combining ids with --all")
	}
}

func TestAgentExportRequiresATarget(t *testing.T) {
	cmd, _ := newAgentExportTestCmd(t)
	err := runAgentExport(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "--all") {
		t.Fatalf("error = %v, want it to mention --all", err)
	}
}

func TestAgentExportFileIsOwnerOnly(t *testing.T) {
	srv := exportMockServer(t, exportableAgent())
	defer srv.Close()
	setCopyTestEnv(t, srv.URL)

	path := filepath.Join(t.TempDir(), "agents.json")
	cmd, _ := newAgentExportTestCmd(t)
	_ = cmd.Flags().Set("file", path)
	if err := runAgentExport(cmd, []string{"agent-src"}); err != nil {
		t.Fatalf("runAgentExport: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, err := decodeAgentConfigDoc(raw); err != nil {
		t.Fatalf("exported file must decode as a document: %v", err)
	}
}
