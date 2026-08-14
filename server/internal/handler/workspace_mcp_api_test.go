package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

const workspaceMcpTestDoc = `{"mcpServers":{"linear":{"url":"https://linear.example","headers":{"Authorization":"Bearer sk-live-workspace"}}}}`

// carriesDocument reports whether a decoded mcp_config field holds an actual
// document. The field is always present on the wire so clients can tell "not
// configured" from "redacted" via the companion flag, and a JSON null decodes
// into json.RawMessage as the literal bytes `null` rather than Go nil.
func carriesDocument(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

// setWorkspaceMcpConfigForTest stores a shared document and restores whatever
// was there afterwards, so these tests can run against the shared fixture
// workspace without leaking state into the rest of the suite.
func setWorkspaceMcpConfigForTest(t *testing.T, doc string) {
	t.Helper()

	ctx := context.Background()
	var previous []byte
	if err := testPool.QueryRow(ctx, `SELECT mcp_config FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&previous); err != nil {
		t.Fatalf("load workspace mcp_config: %v", err)
	}
	// An empty string means "unconfigured" — store SQL NULL, not invalid JSON.
	var next []byte
	if doc != "" {
		next = []byte(doc)
	}
	if _, err := testPool.Exec(ctx, `UPDATE workspace SET mcp_config = $1 WHERE id = $2`, next, testWorkspaceID); err != nil {
		t.Fatalf("set workspace mcp_config: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `UPDATE workspace SET mcp_config = $1 WHERE id = $2`, previous, testWorkspaceID)
	})
}

// setWorkspaceAlwaysRedactForTest flips the workspace-level always-redact
// setting and restores the previous settings blob afterwards.
func setWorkspaceAlwaysRedactForTest(t *testing.T) {
	t.Helper()

	ctx := context.Background()
	var previous []byte
	if err := testPool.QueryRow(ctx, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&previous); err != nil {
		t.Fatalf("load workspace settings: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE workspace SET settings = '{"always_redact_env":true}'::jsonb WHERE id = $1`, testWorkspaceID); err != nil {
		t.Fatalf("set workspace settings: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `UPDATE workspace SET settings = $1 WHERE id = $2`, previous, testWorkspaceID)
	})
}

func getWorkspaceMcpConfigForTest(t *testing.T, mutate func(*http.Request)) (int, WorkspaceMcpConfigResponse) {
	t.Helper()

	req := newRequest(http.MethodGet, "/api/workspaces/"+testWorkspaceID+"/mcp-config", nil)
	req = withURLParam(req, "id", testWorkspaceID)
	if mutate != nil {
		mutate(req)
	}
	w := httptest.NewRecorder()
	testHandler.GetWorkspaceMcpConfig(w, req)

	var resp WorkspaceMcpConfigResponse
	if w.Code == http.StatusOK {
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
	}
	return w.Code, resp
}

func TestGetWorkspaceMcpConfig_OwnerSeesDocument(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	setWorkspaceMcpConfigForTest(t, workspaceMcpTestDoc)

	code, resp := getWorkspaceMcpConfigForTest(t, nil)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if resp.McpConfigRedacted {
		t.Fatal("workspace owner should read the shared document")
	}
	if !bytes.Contains(resp.McpConfig, []byte("sk-live-workspace")) {
		t.Fatalf("expected the stored document, got %s", resp.McpConfig)
	}
	if resp.ServerCount != 1 {
		t.Errorf("server_count = %d, want 1", resp.ServerCount)
	}
}

// An agent actor never reads the shared document, even when the PAT it runs
// under belongs to a workspace owner — the lateral-movement rule mcp_config
// already follows on agents (MUL-2600).
func TestGetWorkspaceMcpConfig_RedactsForAgentActor(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	setWorkspaceMcpConfigForTest(t, workspaceMcpTestDoc)

	caller := createHandlerTestAgent(t, "ws-mcp-caller", nil)
	taskID := insertHandlerTestTask(t, caller)

	code, resp := getWorkspaceMcpConfigForTest(t, func(req *http.Request) {
		req.Header.Set("X-Actor-Source", "task_token")
		req.Header.Set("X-Agent-ID", caller)
		req.Header.Set("X-Task-ID", taskID)
	})
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if !resp.McpConfigRedacted {
		t.Error("expected mcp_config_redacted=true for an agent actor")
	}
	if carriesDocument(resp.McpConfig) {
		t.Errorf("leaked the shared document to an agent actor: %s", resp.McpConfig)
	}
	// The coarse count stays visible: it carries no credential material.
	if resp.ServerCount != 1 {
		t.Errorf("server_count = %d, want 1", resp.ServerCount)
	}
}

func TestGetWorkspaceMcpConfig_RedactsForNonAdminMember(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	setWorkspaceMcpConfigForTest(t, workspaceMcpTestDoc)

	ctx := context.Background()
	var plainUserID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO "user" (name, email)
		VALUES ('Workspace MCP Member', 'ws-mcp-member@multica.test')
		RETURNING id
	`).Scan(&plainUserID); err != nil {
		t.Fatalf("create member user: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM "user" WHERE email = 'ws-mcp-member@multica.test'`)
	})
	if _, err := testPool.Exec(ctx, `
		INSERT INTO member (workspace_id, user_id, role)
		VALUES ($1, $2, 'member')
	`, testWorkspaceID, plainUserID); err != nil {
		t.Fatalf("add workspace member: %v", err)
	}

	code, resp := getWorkspaceMcpConfigForTest(t, func(req *http.Request) {
		req.Header.Set("X-User-ID", plainUserID)
	})
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if !resp.McpConfigRedacted {
		t.Error("expected mcp_config_redacted=true for a plain member")
	}
	if carriesDocument(resp.McpConfig) {
		t.Errorf("leaked the shared document to a plain member: %s", resp.McpConfig)
	}
}

func TestGetWorkspaceMcpConfig_HonorsAlwaysRedactSetting(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	setWorkspaceMcpConfigForTest(t, workspaceMcpTestDoc)
	setWorkspaceAlwaysRedactForTest(t)

	code, resp := getWorkspaceMcpConfigForTest(t, nil)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if !resp.McpConfigRedacted {
		t.Error("always_redact_env must redact even for the workspace owner")
	}
}

func TestGetWorkspaceMcpConfig_UnconfiguredWorkspace(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	setWorkspaceMcpConfigForTest(t, "")

	code, resp := getWorkspaceMcpConfigForTest(t, nil)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if resp.McpConfigRedacted {
		t.Error("an unconfigured workspace is not a redacted one")
	}
	if carriesDocument(resp.McpConfig) || resp.ServerCount != 0 {
		t.Errorf("expected an empty response, got %s / %d servers", resp.McpConfig, resp.ServerCount)
	}
}

func updateWorkspaceMcpConfigForTest(t *testing.T, body any, mutate func(*http.Request)) (int, WorkspaceMcpConfigResponse, string) {
	t.Helper()

	req := newRequest(http.MethodPut, "/api/workspaces/"+testWorkspaceID+"/mcp-config", body)
	req = withURLParam(req, "id", testWorkspaceID)
	if mutate != nil {
		mutate(req)
	}
	w := httptest.NewRecorder()
	testHandler.UpdateWorkspaceMcpConfig(w, req)

	var resp WorkspaceMcpConfigResponse
	raw := w.Body.String()
	if w.Code == http.StatusOK {
		if err := json.Unmarshal([]byte(raw), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
	}
	return w.Code, resp, raw
}

func TestUpdateWorkspaceMcpConfig_SetAndClear(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	setWorkspaceMcpConfigForTest(t, "")

	code, resp, raw := updateWorkspaceMcpConfigForTest(t, map[string]any{
		"mcp_config": json.RawMessage(workspaceMcpTestDoc),
	}, nil)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", code, raw)
	}
	if resp.ServerCount != 1 {
		t.Errorf("server_count = %d, want 1", resp.ServerCount)
	}

	var stored []byte
	if err := testPool.QueryRow(context.Background(), `SELECT mcp_config FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&stored); err != nil {
		t.Fatalf("read back mcp_config: %v", err)
	}
	if !bytes.Contains(stored, []byte("sk-live-workspace")) {
		t.Fatalf("stored document = %s, want the submitted one", stored)
	}

	// An explicit null clears the layer; every inheriting agent falls back to
	// its own config on the next claim.
	code, _, raw = updateWorkspaceMcpConfigForTest(t, map[string]any{"mcp_config": nil}, nil)
	if code != http.StatusOK {
		t.Fatalf("clear: expected 200, got %d: %s", code, raw)
	}
	if err := testPool.QueryRow(context.Background(), `SELECT mcp_config FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&stored); err != nil {
		t.Fatalf("read back cleared mcp_config: %v", err)
	}
	if stored != nil {
		t.Fatalf("expected NULL after clear, got %s", stored)
	}
}

func TestUpdateWorkspaceMcpConfig_RejectsBadShape(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	setWorkspaceMcpConfigForTest(t, "")

	for _, body := range []map[string]any{
		{"mcp_config": []any{}},
		{"mcp_config": "nope"},
		{"mcp_config": map[string]any{"mcpServers": map[string]any{"a": "not-an-object"}}},
		{"mcp_config": map[string]any{"mcp": map[string]any{"a": map[string]any{}}}},
		{},
	} {
		code, _, raw := updateWorkspaceMcpConfigForTest(t, body, nil)
		if code != http.StatusBadRequest {
			t.Errorf("body %v: expected 400, got %d: %s", body, code, raw)
		}
	}

	var stored []byte
	if err := testPool.QueryRow(context.Background(), `SELECT mcp_config FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&stored); err != nil {
		t.Fatalf("read back mcp_config: %v", err)
	}
	if stored != nil {
		t.Fatalf("a rejected write must not touch the stored document, got %s", stored)
	}
}

// Writing shared servers stays a human admin action: a task token that happens
// to run under an owner's PAT must not be able to hand every agent in the
// workspace a new MCP server.
func TestUpdateWorkspaceMcpConfig_DeniesAgentActor(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	setWorkspaceMcpConfigForTest(t, "")

	caller := createHandlerTestAgent(t, "ws-mcp-writer", nil)
	taskID := insertHandlerTestTask(t, caller)

	code, _, raw := updateWorkspaceMcpConfigForTest(t, map[string]any{
		"mcp_config": json.RawMessage(workspaceMcpTestDoc),
	}, func(req *http.Request) {
		req.Header.Set("X-Actor-Source", "task_token")
		req.Header.Set("X-Agent-ID", caller)
		req.Header.Set("X-Task-ID", taskID)
	})
	if code != http.StatusForbidden {
		t.Fatalf("expected 403 for an agent actor, got %d: %s", code, raw)
	}
}
