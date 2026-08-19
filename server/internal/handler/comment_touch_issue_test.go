package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestCreateComment_BumpsIssueActivity pins MUL-5009 and MUL-6343: a new
// comment advances both the legacy updated_at clock and the semantic activity
// clock in the same statement.
func TestCreateComment_BumpsIssueActivity(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	issueID := createCommentTriggerPreviewIssue(t, "comment bumps updated_at", "", "")

	var before, activityBefore time.Time
	if err := testPool.QueryRow(ctx, `SELECT updated_at, last_activity_at FROM issue WHERE id = $1`, issueID).Scan(&before, &activityBefore); err != nil {
		t.Fatalf("read issue clocks before: %v", err)
	}

	// Guarantee a measurable wall-clock gap so the bump is unambiguous; now() is
	// evaluated per statement and the issue was inserted moments ago.
	time.Sleep(10 * time.Millisecond)

	w := httptest.NewRecorder()
	r := withURLParam(
		newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": "a fresh comment"}),
		"id", issueID,
	)
	testHandler.CreateComment(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateComment: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var after, activityAfter time.Time
	if err := testPool.QueryRow(ctx, `SELECT updated_at, last_activity_at FROM issue WHERE id = $1`, issueID).Scan(&after, &activityAfter); err != nil {
		t.Fatalf("read issue clocks after: %v", err)
	}

	if !after.After(before) {
		t.Fatalf("issue updated_at was not bumped by a new comment: before=%s after=%s", before, after)
	}
	if !activityAfter.After(activityBefore) {
		t.Fatalf("issue last_activity_at was not bumped by a new comment: before=%s after=%s", activityBefore, activityAfter)
	}
}

// TestCreateComment_WorkspaceMismatchPersistsNothing pins the tenant-integrity
// guarantee of the CreateComment CTE (MUL-5009 nit2): CreateComment is the
// single carrier of "a comment always belongs to an issue in the same
// workspace and always bumps it". If the passed workspace does not match the
// target issue's workspace, the leading UPDATE matches no issue row, the
// dependent INSERT selects nothing, and the :one query returns pgx.ErrNoRows —
// so no mis-attributed comment is written and the issue is not touched.
func TestCreateComment_WorkspaceMismatchPersistsNothing(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	issueID := createCommentTriggerPreviewIssue(t, "comment workspace guard", "", "")

	var before, activityBefore time.Time
	if err := testPool.QueryRow(ctx, `SELECT updated_at, last_activity_at FROM issue WHERE id = $1`, issueID).Scan(&before, &activityBefore); err != nil {
		t.Fatalf("read issue clocks before: %v", err)
	}

	// A workspace that is NOT the issue's workspace: the issue exists, but the
	// (issue, workspace) pair matches no row.
	wrongWorkspace := parseUUID("11111111-1111-1111-1111-111111111111")
	_, err := testHandler.Queries.CreateComment(ctx, db.CreateCommentParams{
		IssueID:     parseUUID(issueID),
		WorkspaceID: wrongWorkspace,
		AuthorType:  "member",
		AuthorID:    parseUUID(testUserID),
		Content:     "should never persist",
		Type:        "comment",
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expected pgx.ErrNoRows on workspace mismatch, got %v", err)
	}

	var commentCount int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM comment WHERE issue_id = $1`, issueID).Scan(&commentCount); err != nil {
		t.Fatalf("count comments: %v", err)
	}
	if commentCount != 0 {
		t.Fatalf("workspace mismatch must persist no comment, found %d", commentCount)
	}

	var after, activityAfter time.Time
	if err := testPool.QueryRow(ctx, `SELECT updated_at, last_activity_at FROM issue WHERE id = $1`, issueID).Scan(&after, &activityAfter); err != nil {
		t.Fatalf("read issue clocks after: %v", err)
	}
	if !after.Equal(before) {
		t.Fatalf("workspace mismatch must not bump updated_at: before=%s after=%s", before, after)
	}
	if !activityAfter.Equal(activityBefore) {
		t.Fatalf("workspace mismatch must not bump last_activity_at: before=%s after=%s", activityBefore, activityAfter)
	}
}

func TestUpdateAndDeleteCommentBumpIssueActivity(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	issueID := createCommentTriggerPreviewIssue(t, "comment mutations bump activity", "", "")
	comment, err := testHandler.Queries.CreateComment(ctx, db.CreateCommentParams{
		IssueID:     parseUUID(issueID),
		WorkspaceID: parseUUID(testWorkspaceID),
		AuthorType:  "member",
		AuthorID:    parseUUID(testUserID),
		Content:     "before",
		Type:        "comment",
	})
	if err != nil {
		t.Fatalf("CreateComment: %v", err)
	}

	base := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
	setBase := func() {
		t.Helper()
		if _, err := testPool.Exec(ctx, `UPDATE issue SET last_activity_at = $2 WHERE id = $1`, issueID, base); err != nil {
			t.Fatalf("reset last_activity_at: %v", err)
		}
	}
	readActivity := func() time.Time {
		t.Helper()
		var got time.Time
		if err := testPool.QueryRow(ctx, `SELECT last_activity_at FROM issue WHERE id = $1`, issueID).Scan(&got); err != nil {
			t.Fatalf("read last_activity_at: %v", err)
		}
		return got
	}

	setBase()
	if _, err := testHandler.Queries.UpdateComment(ctx, db.UpdateCommentParams{
		ID:      comment.ID,
		Content: "after",
	}); err != nil {
		t.Fatalf("UpdateComment: %v", err)
	}
	if got := readActivity(); !got.After(base) {
		t.Fatalf("comment edit did not advance activity: base=%s got=%s", base, got)
	}

	setBase()
	if err := testHandler.Queries.DeleteComment(ctx, db.DeleteCommentParams{
		ID:          comment.ID,
		WorkspaceID: parseUUID(testWorkspaceID),
	}); err != nil {
		t.Fatalf("DeleteComment: %v", err)
	}
	if got := readActivity(); !got.After(base) {
		t.Fatalf("comment delete did not advance activity: base=%s got=%s", base, got)
	}
}
