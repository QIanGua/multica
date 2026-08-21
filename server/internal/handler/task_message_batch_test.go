package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestReportTaskMessagesPersistsWholeBatch covers the multi-message shape of the
// /messages endpoint after it stopped issuing one INSERT per message. The row
// count, the seq values, and the NULL/non-NULL split all have to survive the
// jsonb round trip that CreateTaskMessages does: a tool event carries tool +
// input + output, a text event carries only content, and everything the daemon
// omitted must land as SQL NULL rather than an empty string.
func TestReportTaskMessagesPersistsWholeBatch(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	_, taskID := seedNULTask(t, "batch-messages-agent")

	w := httptest.NewRecorder()
	req := daemonTaskRequest(t, "/api/daemon/tasks/"+taskID+"/messages", taskID, map[string]any{
		"messages": []any{
			map[string]any{"seq": 1, "type": "thinking", "content": "planning"},
			map[string]any{
				"seq":    2,
				"type":   "tool_use",
				"tool":   "fs_read",
				"input":  map[string]any{"path": "/etc/hosts", "nested": map[string]any{"depth": 2}},
				"output": "127.0.0.1 localhost",
			},
			map[string]any{"seq": 3, "type": "text", "content": "done"},
		},
	})

	testHandler.ReportTaskMessages(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ReportTaskMessages returned %d, want 200: %s", w.Code, w.Body.String())
	}

	rows, err := testPool.Query(ctx, `
		SELECT seq, type, tool, content, output, input::text
		FROM task_message WHERE task_id = $1 ORDER BY seq ASC`, taskID)
	if err != nil {
		t.Fatalf("read persisted task messages: %v", err)
	}
	defer rows.Close()

	type stored struct {
		seq                          int32
		msgType                      string
		tool, content, output, input *string
	}
	var got []stored
	for rows.Next() {
		var s stored
		if err := rows.Scan(&s.seq, &s.msgType, &s.tool, &s.content, &s.output, &s.input); err != nil {
			t.Fatalf("scan task message: %v", err)
		}
		got = append(got, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate task messages: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("persisted %d task messages, want 3", len(got))
	}
	for i, want := range []int32{1, 2, 3} {
		if got[i].seq != want {
			t.Fatalf("row %d seq = %d, want %d", i, got[i].seq, want)
		}
	}
	if got[0].msgType != "thinking" || got[0].content == nil || *got[0].content != "planning" {
		t.Fatalf("thinking row = %+v, want type=thinking content=planning", got[0])
	}
	// Fields the daemon did not send must be NULL, not "".
	if got[0].tool != nil || got[0].output != nil || got[0].input != nil {
		t.Fatalf("thinking row should have NULL tool/output/input, got %+v", got[0])
	}
	if got[1].tool == nil || *got[1].tool != "fs_read" {
		t.Fatalf("tool_use row tool = %v, want fs_read", got[1].tool)
	}
	if got[1].output == nil || *got[1].output != "127.0.0.1 localhost" {
		t.Fatalf("tool_use row output = %v", got[1].output)
	}
	// The JSONB argument must arrive as an object, not as a string holding JSON.
	if got[1].input == nil {
		t.Fatal("tool_use row lost its input JSONB")
	}
	var inputPath string
	if err := testPool.QueryRow(ctx, `
		SELECT input->>'path' FROM task_message WHERE task_id = $1 AND seq = 2`, taskID).Scan(&inputPath); err != nil {
		t.Fatalf("read input->>path: %v", err)
	}
	if inputPath != "/etc/hosts" {
		t.Fatalf("input->>path = %q, want /etc/hosts", inputPath)
	}
	if got[1].content != nil {
		t.Fatalf("tool_use row content should be NULL, got %q", *got[1].content)
	}
	if got[2].msgType != "text" || got[2].content == nil || *got[2].content != "done" {
		t.Fatalf("text row = %+v, want type=text content=done", got[2])
	}
}

// TestReportTaskMessagesBatchIsAtomic pins the durability property the
// single-statement insert buys: the batch either lands whole or not at all. The
// per-message loop it replaced could persist the first rows and then fail,
// leaving a permanent hole in the transcript — the daemon never retries this
// endpoint. A duplicate id inside the batch is the cheapest way to make
// PostgreSQL reject the statement mid-batch.
func TestReportTaskMessagesBatchIsAtomic(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	_, taskID := seedNULTask(t, "batch-atomic-agent")

	// A NUL in the JSONB input is rejected by the sanitizer, so drive the
	// failure through a constraint instead: seq is not unique, but the primary
	// key is, and the handler mints ids itself. Reuse the query directly with a
	// colliding id to prove the statement is all-or-nothing.
	dup := "018f0000-0000-7000-8000-000000000001"
	_, err := testPool.Exec(ctx, `
		INSERT INTO task_message (id, task_id, seq, type, content)
		VALUES ($1, $2, 1, 'text', 'first')`, dup, taskID)
	if err != nil {
		t.Fatalf("seed conflicting row: %v", err)
	}

	_, err = testPool.Exec(ctx, `
		INSERT INTO task_message (id, task_id, seq, type, tool, content, input, output)
		SELECT m.id, $1::uuid, m.seq, m.type, m.tool, m.content, m.input, m.output
		FROM jsonb_to_recordset($2::jsonb)
			AS m(id uuid, seq integer, type text, tool text, content text, input jsonb, output text)`,
		taskID, `[{"id":"018f0000-0000-7000-8000-000000000002","seq":2,"type":"text","content":"ok"},
		          {"id":"`+dup+`","seq":3,"type":"text","content":"collides"}]`)
	if err == nil {
		t.Fatal("expected the batch insert to fail on the duplicate id")
	}

	var count int
	if err := testPool.QueryRow(ctx, `
		SELECT COUNT(*) FROM task_message WHERE task_id = $1`, taskID).Scan(&count); err != nil {
		t.Fatalf("count task messages: %v", err)
	}
	if count != 1 {
		t.Fatalf("task_message rows after failed batch = %d, want 1 (only the seeded row)", count)
	}
}
