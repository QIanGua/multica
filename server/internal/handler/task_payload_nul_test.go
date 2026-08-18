package handler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// seedNULTask creates an agent with one running task, ready to be driven
// through a terminal daemon callback.
func seedNULTask(t *testing.T, label string) (agentID, taskID string) {
	t.Helper()
	ctx := context.Background()

	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent (workspace_id, name, description, runtime_mode, runtime_config,
			runtime_id, visibility, max_concurrent_tasks, owner_id,
			instructions, custom_env, custom_args)
		VALUES ($1, $2, '', 'cloud', '{}'::jsonb, $3, 'private', 1, $4, '', '{}'::jsonb, '[]'::jsonb)
		RETURNING id`, testWorkspaceID, label, handlerTestRuntimeID(t), testUserID).Scan(&agentID); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent WHERE id = $1`, agentID)
	})

	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, status, priority, started_at)
		VALUES ($1, $2, 'running', 0, now())
		RETURNING id`, agentID, handlerTestRuntimeID(t)).Scan(&taskID); err != nil {
		t.Fatalf("seed running task: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID)
	})
	return agentID, taskID
}

// TestCompleteTaskPayloadWithNULPersists is the /complete half of GH #7098.
//
// agent_task_queue.result is JSONB, and encoding/json renders an embedded NUL
// as a \\u0000 escape, which PostgreSQL refuses to convert to text
// (SQLSTATE 22P05). The control leg proves the database really does reject the
// raw payload -- without it this test would still pass if the sanitizer were
// deleted, and the regression would walk straight back in.
func TestCompleteTaskPayloadWithNULPersists(t *testing.T) {
	ctx := context.Background()
	_, taskID := seedNULTask(t, "nul-complete-agent")

	req := TaskCompleteRequest{
		Output:    "done\x00 summary text",
		WorkDir:   "/tmp/work\x00dir",
		SessionID: "sess\x00ion",
	}

	// Control: exactly what shipped before the fix.
	rawResult, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal raw request: %v", err)
	}
	if !strings.Contains(string(rawResult), "\\u0000") {
		t.Fatalf("premise broken: raw payload carries no NUL escape: %s", rawResult)
	}
	if _, err := testHandler.Queries.CompleteAgentTask(ctx, db.CompleteAgentTaskParams{
		ID:     util.MustParseUUID(taskID),
		Result: rawResult,
	}); err == nil {
		t.Fatal("premise broken: PostgreSQL accepted a JSONB payload containing a NUL escape")
	}

	// The task must still be running -- the rejected write rolled back, which
	// is exactly the wedge users reported.
	var status string
	if err := testPool.QueryRow(ctx, `SELECT status FROM agent_task_queue WHERE id = $1`, taskID).Scan(&status); err != nil {
		t.Fatalf("read task after rejected write: %v", err)
	}
	if status != "running" {
		t.Fatalf("status after rejected write = %q, want running", status)
	}

	// Now the fixed path.
	sanitizeTaskCompleteRequest(&req)
	cleanResult, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal sanitized request: %v", err)
	}
	if strings.Contains(string(cleanResult), "\\u0000") {
		t.Fatalf("sanitized payload still carries a NUL escape: %s", cleanResult)
	}
	if _, err := testHandler.Queries.CompleteAgentTask(ctx, db.CompleteAgentTaskParams{
		ID:     util.MustParseUUID(taskID),
		Result: cleanResult,
	}); err != nil {
		t.Fatalf("CompleteAgentTask with sanitized payload: %v", err)
	}

	var (
		gotStatus   string
		completedAt *string
		storedJSON  []byte
	)
	if err := testPool.QueryRow(ctx, `
		SELECT status, completed_at::text, result
		FROM agent_task_queue WHERE id = $1`, taskID).Scan(&gotStatus, &completedAt, &storedJSON); err != nil {
		t.Fatalf("read completed task: %v", err)
	}
	if gotStatus != "completed" {
		t.Fatalf("status = %q, want completed", gotStatus)
	}
	if completedAt == nil {
		t.Fatal("completed_at is NULL, want a terminal timestamp")
	}

	var stored TaskCompleteRequest
	if err := json.Unmarshal(storedJSON, &stored); err != nil {
		t.Fatalf("decode stored result: %v", err)
	}
	// The whole point of removing rather than rejecting: the readable
	// diagnostic survives.
	if stored.Output != "done summary text" {
		t.Fatalf("stored output = %q, want %q", stored.Output, "done summary text")
	}
	if stored.WorkDir != "/tmp/workdir" {
		t.Fatalf("stored work_dir = %q, want %q", stored.WorkDir, "/tmp/workdir")
	}
}

// TestTaskMessageNestedInputWithNULPersists covers the entry Elon flagged:
// task_messages.input is JSONB, so a NUL nested anywhere inside a tool's
// arguments breaks the insert even when every top-level string is clean.
// This endpoint has no daemon-side retry, so an unguarded batch is lost
// silently.
func TestTaskMessageNestedInputWithNULPersists(t *testing.T) {
	ctx := context.Background()
	_, taskID := seedNULTask(t, "nul-messages-agent")

	newInput := func() map[string]any {
		return map[string]any{
			"command": "cat",
			"args":    []any{"-n", "build/app.bin"},
			"result": map[string]any{
				"stdout": "ELF\x00\x00binary",
			},
		}
	}

	// Control: top-level strings are all clean, the poison is two levels down.
	rawInput, err := json.Marshal(newInput())
	if err != nil {
		t.Fatalf("marshal raw input: %v", err)
	}
	if _, err := testHandler.Queries.CreateTaskMessage(ctx, db.CreateTaskMessageParams{
		TaskID: util.MustParseUUID(taskID),
		Seq:    1,
		Type:   "tool_use",
		Input:  rawInput,
	}); err == nil {
		t.Fatal("premise broken: PostgreSQL accepted nested JSONB containing a NUL escape")
	}

	// Fixed path: the deep walk reaches it.
	cleaned, ok := util.SanitizeJSONForPostgres(newInput()).(map[string]any)
	if !ok {
		t.Fatal("SanitizeJSONForPostgres did not return an object")
	}
	cleanInput, err := json.Marshal(cleaned)
	if err != nil {
		t.Fatalf("marshal sanitized input: %v", err)
	}
	if strings.Contains(string(cleanInput), "\\u0000") {
		t.Fatalf("sanitized input still carries a NUL escape: %s", cleanInput)
	}
	created, err := testHandler.Queries.CreateTaskMessage(ctx, db.CreateTaskMessageParams{
		TaskID: util.MustParseUUID(taskID),
		Seq:    2,
		Type:   "tool_use",
		Input:  cleanInput,
	})
	if err != nil {
		t.Fatalf("CreateTaskMessage with sanitized input: %v", err)
	}

	var stored struct {
		Result struct {
			Stdout string `json:"stdout"`
		} `json:"result"`
	}
	if err := json.Unmarshal(created.Input, &stored); err != nil {
		t.Fatalf("decode stored input: %v", err)
	}
	if stored.Result.Stdout != "ELFbinary" {
		t.Fatalf("stored nested stdout = %q, want %q", stored.Result.Stdout, "ELFbinary")
	}
}
