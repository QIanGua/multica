package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These tests pin the authorization contract of RecordSquadLeaderEvaluation
// after MUL-6622 / GH #7487: the leader turn is proven by the TASK row
// (is_leader_task + squad_id + agent_id), never by the target issue's assignee.

// insertIssueWithAssignee creates a workspace issue owned by an individual agent
// — i.e. NOT assigned to any squad — and returns its id.
func insertIssueWithAssignee(t *testing.T, title, assigneeType, assigneeID string) string {
	t.Helper()

	// issue.number is assigned by application code, so a raw INSERT would reuse
	// the 0 default and collide with uq_issue_workspace_number as soon as a test
	// creates a second issue in this workspace.
	var issueID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO issue (workspace_id, creator_type, creator_id, title, assignee_type, assignee_id, number)
		VALUES ($1, 'member', $2, $3, $4, $5,
		        (SELECT COALESCE(MAX(number), 0) + 1 FROM issue WHERE workspace_id = $1))
		RETURNING id
	`, testWorkspaceID, testUserID, title, assigneeType, assigneeID).Scan(&issueID); err != nil {
		t.Fatalf("create issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
	})
	return issueID
}

// insertLeaderTaskOnIssue creates a running task for agentID bound to issueID.
// isLeaderTask / squadID control the exact provenance under test; an empty
// squadID leaves squad_id NULL.
func insertLeaderTaskOnIssue(t *testing.T, agentID, issueID string, isLeaderTask bool, squadID string) string {
	t.Helper()
	ctx := context.Background()

	var runtimeID string
	if err := testPool.QueryRow(ctx, `SELECT runtime_id FROM agent WHERE id = $1`, agentID).Scan(&runtimeID); err != nil {
		t.Fatalf("load agent runtime: %v", err)
	}

	var squadArg any
	if squadID != "" {
		squadArg = squadID
	}

	var taskID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (
			agent_id, runtime_id, issue_id, status, priority, started_at,
			is_leader_task, squad_id
		)
		VALUES ($1, $2, $3, 'running', 0, now(), $4, $5)
		RETURNING id
	`, agentID, runtimeID, issueID, isLeaderTask, squadArg).Scan(&taskID); err != nil {
		t.Fatalf("create task: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID)
	})
	return taskID
}

func postSquadEvaluation(t *testing.T, issueID, agentID, taskID, outcome string) *httptest.ResponseRecorder {
	t.Helper()

	w := httptest.NewRecorder()
	r := newRequest("POST", "/api/issues/"+issueID+"/squad-evaluated", map[string]any{
		"outcome": outcome,
		"reason":  "test reason",
	})
	r = withURLParam(r, "id", issueID)
	r.Header.Set("X-Agent-ID", agentID)
	r.Header.Set("X-Task-ID", taskID)

	testHandler.RecordSquadLeaderEvaluation(w, r)
	return w
}

type recordedEvaluation struct {
	ActorID string
	SquadID string
	Outcome string
}

func loadEvaluations(t *testing.T, issueID string) []recordedEvaluation {
	t.Helper()

	rows, err := testPool.Query(context.Background(), `
		SELECT actor_id, details->>'squad_id', details->>'outcome'
		FROM activity_log
		WHERE issue_id = $1 AND action = 'squad_leader_evaluated'
		ORDER BY created_at ASC
	`, issueID)
	if err != nil {
		t.Fatalf("load evaluations: %v", err)
	}
	defer rows.Close()

	var out []recordedEvaluation
	for rows.Next() {
		var e recordedEvaluation
		if err := rows.Scan(&e.ActorID, &e.SquadID, &e.Outcome); err != nil {
			t.Fatalf("scan evaluation: %v", err)
		}
		out = append(out, e)
	}
	return out
}

func errorMessage(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	msg, _ := body["error"].(string)
	return msg
}

// The regression: a leader task on an issue owned by an individual agent (the
// `@squad`-mention path) used to be rejected with "issue is not assigned to a
// squad", dropping the decision entirely.
func TestRecordSquadLeaderEvaluation_AcceptedOnNonSquadAssignedIssue(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	fx := newSquadCommentTriggerFixture(t)
	issueID := insertIssueWithAssignee(t, "agent-owned issue", "agent", fx.OtherID)
	taskID := insertLeaderTaskOnIssue(t, fx.LeaderID, issueID, true, fx.SquadID)

	w := postSquadEvaluation(t, issueID, fx.LeaderID, taskID, "no_action")
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 on non-squad-assigned issue, got %d: %s", w.Code, w.Body.String())
	}

	got := loadEvaluations(t, issueID)
	if len(got) != 1 {
		t.Fatalf("expected exactly one recorded evaluation, got %d", len(got))
	}
	if got[0].ActorID != fx.LeaderID {
		t.Fatalf("actor_id: want task agent %s, got %s", fx.LeaderID, got[0].ActorID)
	}
	if got[0].SquadID != fx.SquadID {
		t.Fatalf("details.squad_id: want task squad %s, got %s", fx.SquadID, got[0].SquadID)
	}
	if got[0].Outcome != "no_action" {
		t.Fatalf("outcome: want no_action, got %s", got[0].Outcome)
	}
}

// A child issue the leader itself is running on records fine too — the parent's
// squad assignment is irrelevant to the check.
func TestRecordSquadLeaderEvaluation_AcceptedOnChildIssueBoundTask(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	fx := newSquadCommentTriggerFixture(t)
	childID := insertIssueWithAssignee(t, "squad child issue", "agent", fx.OtherID)
	if _, err := testPool.Exec(context.Background(),
		`UPDATE issue SET parent_issue_id = $1 WHERE id = $2`, uuidToString(fx.Issue.ID), childID); err != nil {
		t.Fatalf("link child to parent: %v", err)
	}
	taskID := insertLeaderTaskOnIssue(t, fx.LeaderID, childID, true, fx.SquadID)

	if w := postSquadEvaluation(t, childID, fx.LeaderID, taskID, "action"); w.Code != http.StatusCreated {
		t.Fatalf("expected 201 on child issue, got %d: %s", w.Code, w.Body.String())
	}
	if got := loadEvaluations(t, childID); len(got) != 1 {
		t.Fatalf("expected one recorded evaluation on the child, got %d", len(got))
	}
}

// Behavior narrowing made explicit: the leader agent running a task that is NOT
// a leader task is not running as the leader, so it may not record.
func TestRecordSquadLeaderEvaluation_RejectsNonLeaderTask(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	fx := newSquadCommentTriggerFixture(t)
	issueID := uuidToString(fx.Issue.ID)
	taskID := insertLeaderTaskOnIssue(t, fx.LeaderID, issueID, false, fx.SquadID)

	w := postSquadEvaluation(t, issueID, fx.LeaderID, taskID, "no_action")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a non-leader task, got %d: %s", w.Code, w.Body.String())
	}
	if got := loadEvaluations(t, issueID); len(got) != 0 {
		t.Fatalf("expected no evaluation recorded, got %d", len(got))
	}
}

// A leader task without a stamped squad cannot be attributed to a squad.
func TestRecordSquadLeaderEvaluation_RejectsLeaderTaskWithoutSquadID(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	fx := newSquadCommentTriggerFixture(t)
	issueID := uuidToString(fx.Issue.ID)
	taskID := insertLeaderTaskOnIssue(t, fx.LeaderID, issueID, true, "")

	w := postSquadEvaluation(t, issueID, fx.LeaderID, taskID, "no_action")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a leader task with no squad_id, got %d: %s", w.Code, w.Body.String())
	}
	if got := loadEvaluations(t, issueID); len(got) != 0 {
		t.Fatalf("expected no evaluation recorded, got %d", len(got))
	}
}

// Recording still binds to the task's own issue, and the error names it — the
// stage-barrier case wakes the leader on the PARENT, so a leader that reaches
// for the child id gets told where to record instead of a dead end.
func TestRecordSquadLeaderEvaluation_RejectsCrossIssueTaskAndNamesTaskIssue(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	fx := newSquadCommentTriggerFixture(t)
	parentID := uuidToString(fx.Issue.ID)
	childID := insertIssueWithAssignee(t, "stage barrier child", "agent", fx.OtherID)
	taskID := insertLeaderTaskOnIssue(t, fx.LeaderID, parentID, true, fx.SquadID)

	w := postSquadEvaluation(t, childID, fx.LeaderID, taskID, "no_action")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a cross-issue task, got %d: %s", w.Code, w.Body.String())
	}
	if msg := errorMessage(t, w); !strings.Contains(msg, parentID) {
		t.Fatalf("expected the error to name the task's issue %s, got %q", parentID, msg)
	}
	if got := loadEvaluations(t, childID); len(got) != 0 {
		t.Fatalf("expected no evaluation recorded on the child, got %d", len(got))
	}
}

// An agent that is not the task's agent may not record on its behalf.
func TestRecordSquadLeaderEvaluation_RejectsForeignAgent(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	fx := newSquadCommentTriggerFixture(t)
	issueID := uuidToString(fx.Issue.ID)
	taskID := insertLeaderTaskOnIssue(t, fx.LeaderID, issueID, true, fx.SquadID)

	w := postSquadEvaluation(t, issueID, fx.OtherID, taskID, "no_action")
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a non-task agent, got %d: %s", w.Code, w.Body.String())
	}
	if got := loadEvaluations(t, issueID); len(got) != 0 {
		t.Fatalf("expected no evaluation recorded, got %d", len(got))
	}
}

// A leader handover mid-run must not discard the record or misattribute it: the
// activity actor stays the task's agent, which is what the no_action comment
// suppression lookup matches on.
func TestRecordSquadLeaderEvaluation_SurvivesLeaderChangeAndKeepsNoActionSuppression(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	fx := newRunningSquadLeaderTaskFixture(t)
	newLeaderID := createHandlerTestAgent(t, "Squad New Leader", nil)
	if _, err := testPool.Exec(context.Background(),
		`UPDATE squad SET leader_id = $1 WHERE id = $2`, newLeaderID, fx.SquadID); err != nil {
		t.Fatalf("rotate squad leader: %v", err)
	}

	w := postSquadEvaluation(t, fx.IssueID, fx.LeaderID, fx.TaskID, "no_action")
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 after a leader change, got %d: %s", w.Code, w.Body.String())
	}

	got := loadEvaluations(t, fx.IssueID)
	if len(got) != 1 {
		t.Fatalf("expected one recorded evaluation, got %d", len(got))
	}
	if got[0].ActorID != fx.LeaderID {
		t.Fatalf("actor_id: want the task's agent %s, got %s", fx.LeaderID, got[0].ActorID)
	}

	// Suppression must still fire for this task.
	cw := httptest.NewRecorder()
	cr := newRequest("POST", "/api/issues/"+fx.IssueID+"/comments", map[string]any{
		"content":   "No action needed.",
		"parent_id": fx.TriggerCommentID,
	})
	cr = withURLParam(cr, "id", fx.IssueID)
	cr.Header.Set("X-Agent-ID", fx.LeaderID)
	cr.Header.Set("X-Task-ID", fx.TaskID)

	testHandler.CreateComment(cw, cr)
	if cw.Code != http.StatusConflict {
		t.Fatalf("expected the no_action comment to stay suppressed after a leader change, got %d: %s", cw.Code, cw.Body.String())
	}
}
