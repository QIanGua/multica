package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Board, list and swimlane columns are CATEGORIES, not status keys (MUL-6243).
// These tests pin the server half of that contract: a custom status must land
// in its category's column, count toward it, and be reachable through the
// paginated row query — the exact path where the first cut of the UI dropped
// every custom-status card on the floor.
func seedStatusCategoryFixture(t *testing.T) (projectID, customKey string) {
	t.Helper()
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	customKey = fmt.Sprintf("qa_%d", suffix)

	if err := testPool.QueryRow(ctx, `
		INSERT INTO project (workspace_id, title)
		VALUES ($1, $2)
		RETURNING id
	`, testWorkspaceID, fmt.Sprintf("Status category %d", suffix)).Scan(&projectID); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO issue_status (workspace_id, key, name, description, category, color, position)
		VALUES ($1, $2, 'QA', '', 'in_review', '#ff0000', 1)
	`, testWorkspaceID, customKey); err != nil {
		t.Fatalf("create custom status: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM issue WHERE project_id = $1`, projectID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM project WHERE id = $1`, projectID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM issue_status WHERE workspace_id = $1 AND key = $2`, testWorkspaceID, customKey)
	})

	var firstNumber int
	if err := testPool.QueryRow(ctx, `
		UPDATE workspace
		SET issue_counter = GREATEST(
			issue_counter,
			(SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)
		) + 4
		WHERE id = $1
		RETURNING issue_counter - 3
	`, testWorkspaceID).Scan(&firstNumber); err != nil {
		t.Fatalf("reserve issue numbers: %v", err)
	}
	// Two on the custom status, one on the built-in it behaves as, one elsewhere.
	if _, err := testPool.Exec(ctx, `
		INSERT INTO issue (workspace_id, title, status, priority, creator_type, creator_id, position, number, project_id)
		VALUES
			($1, 'cat-a', $2,           'none', 'member', $3, 1, $4,     $5),
			($1, 'cat-b', $2,           'none', 'member', $3, 2, $4 + 1, $5),
			($1, 'cat-c', 'in_review',  'none', 'member', $3, 3, $4 + 2, $5),
			($1, 'cat-d', 'todo',       'none', 'member', $3, 4, $4 + 3, $5)
	`, testWorkspaceID, customKey, testUserID, firstNumber, projectID); err != nil {
		t.Fatalf("seed issues: %v", err)
	}
	return projectID, customKey
}

func statusCategoryQuery(projectID string) issueTableQuerySpec {
	return issueTableQuerySpec{
		Scope:   issueTableScope{Kind: "project", ProjectID: projectID},
		Filters: issueTableFiltersRequest{},
		Sort:    issueTableSortRequest{Field: "title", Direction: "asc"},
	}
}

func TestIssueTableStatusCategoryGroupsFoldCustomStatuses(t *testing.T) {
	projectID, _ := seedStatusCategoryFixture(t)

	w := httptest.NewRecorder()
	testHandler.ListIssueTableGroups(w, newRequest(http.MethodPost, "/api/issues/table/groups", issueTableGroupsRequest{
		Query: statusCategoryQuery(projectID),
		Group: issueTableGroupSpec{Kind: "status_category"},
		Page:  issueTablePageRequest{Limit: 100},
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("groups status = %d: %s", w.Code, w.Body.String())
	}
	var groups issueTableGroupsResponse
	if err := json.NewDecoder(w.Body).Decode(&groups); err != nil {
		t.Fatalf("decode groups: %v", err)
	}

	counts := map[string]int64{}
	for _, group := range groups.Groups {
		counts[group.Key] = group.Count
		// A custom status must never surface a column of its own.
		if group.Key != statusCategoryGroupKey(group.Value.Status) {
			t.Fatalf("group %q is not a category column: %#v", group.Key, group.Value)
		}
	}
	if got := counts[statusCategoryGroupKey("in_review")]; got != 3 {
		t.Fatalf("in_review category count = %d, want 3 (2 custom + 1 built-in): %#v", got, counts)
	}
	if got := counts[statusCategoryGroupKey("todo")]; got != 1 {
		t.Fatalf("todo category count = %d, want 1: %#v", got, counts)
	}
	if groups.Total != 4 {
		t.Fatalf("total = %d, want 4", groups.Total)
	}
}

func TestIssueTableStatusCategoryRowsReturnCustomStatusIssues(t *testing.T) {
	projectID, customKey := seedStatusCategoryFixture(t)

	groupKey := statusCategoryGroupKey("in_review")
	w := httptest.NewRecorder()
	testHandler.ListIssueTableRows(w, newRequest(http.MethodPost, "/api/issues/table/rows", issueTableRowsRequest{
		Query:    statusCategoryQuery(projectID),
		Group:    issueTableGroupSpec{Kind: "status_category"},
		GroupKey: &groupKey,
		Page:     issueTablePageRequest{Limit: 50},
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("rows status = %d: %s", w.Code, w.Body.String())
	}
	var rows issueTableRowsResponse
	if err := json.NewDecoder(w.Body).Decode(&rows); err != nil {
		t.Fatalf("decode rows: %v", err)
	}

	byStatus := map[string]int{}
	for _, row := range rows.Rows {
		byStatus[row.Issue.Status]++
	}
	// The regression: paging the in_review column by concrete key returned only
	// the built-in row, so both QA cards vanished from the board.
	if byStatus[customKey] != 2 {
		t.Fatalf("custom-status rows = %d, want 2: %#v", byStatus[customKey], byStatus)
	}
	if byStatus["in_review"] != 1 {
		t.Fatalf("built-in rows = %d, want 1: %#v", byStatus["in_review"], byStatus)
	}
	if len(rows.Rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows.Rows))
	}
	// Every row carries its category, so the client can render the column
	// without a second catalog round-trip.
	for _, row := range rows.Rows {
		if row.Issue.StatusCategory != "in_review" {
			t.Fatalf("row %q status_category = %q, want in_review", row.Issue.Title, row.Issue.StatusCategory)
		}
	}
}

func TestIssueTableCompoundStatusCategoryCellsFoldCustomStatuses(t *testing.T) {
	projectID, customKey := seedStatusCategoryFixture(t)

	w := httptest.NewRecorder()
	testHandler.ListIssueTableGroups(w, newRequest(http.MethodPost, "/api/issues/table/groups", issueTableGroupsRequest{
		Query: statusCategoryQuery(projectID),
		Group: issueTableGroupSpec{
			Kind:      "compound",
			Primary:   "project",
			Secondary: "status_category",
		},
		Page: issueTablePageRequest{Limit: 100},
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("groups status = %d: %s", w.Code, w.Body.String())
	}
	var groups issueTableGroupsResponse
	if err := json.NewDecoder(w.Body).Decode(&groups); err != nil {
		t.Fatalf("decode groups: %v", err)
	}
	if len(groups.Groups) != 1 {
		t.Fatalf("groups = %d, want 1 project lane", len(groups.Groups))
	}

	cells := map[string]int64{}
	for _, cell := range groups.Groups[0].SecondaryGroups {
		cells[cell.Value.Status] = cell.Count
	}
	// Swimlane cells are categories too: 2 QA + 1 In Review in one cell, and no
	// cell keyed by the custom status.
	if cells["in_review"] != 3 {
		t.Fatalf("in_review cell = %d, want 3: %#v", cells["in_review"], cells)
	}
	if _, exists := cells[customKey]; exists {
		t.Fatalf("custom status got its own swimlane cell: %#v", cells)
	}

	// And the cell's own group_key has to page back the same three rows.
	cellKey := compoundCellGroupKey(groups.Groups[0].Key, "in_review", true)
	rowsRecorder := httptest.NewRecorder()
	testHandler.ListIssueTableRows(rowsRecorder, newRequest(http.MethodPost, "/api/issues/table/rows", issueTableRowsRequest{
		Query: statusCategoryQuery(projectID),
		Group: issueTableGroupSpec{
			Kind:      "compound",
			Primary:   "project",
			Secondary: "status_category",
		},
		GroupKey: &cellKey,
		Page:     issueTablePageRequest{Limit: 50},
	}))
	if rowsRecorder.Code != http.StatusOK {
		t.Fatalf("cell rows status = %d: %s", rowsRecorder.Code, rowsRecorder.Body.String())
	}
	var cellRows issueTableRowsResponse
	if err := json.NewDecoder(rowsRecorder.Body).Decode(&cellRows); err != nil {
		t.Fatalf("decode cell rows: %v", err)
	}
	if len(cellRows.Rows) != 3 {
		t.Fatalf("cell rows = %d, want 3", len(cellRows.Rows))
	}
}

// A workspace with no custom statuses must produce byte-identical grouping to
// the plain status contract — that is what makes this safe to ship default-on.
func TestIssueTableStatusCategoryMatchesStatusWithoutCustomStatuses(t *testing.T) {
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	var projectID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO project (workspace_id, title) VALUES ($1, $2) RETURNING id
	`, testWorkspaceID, fmt.Sprintf("Status category parity %d", suffix)).Scan(&projectID); err != nil {
		t.Fatalf("create project: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM issue WHERE project_id = $1`, projectID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM project WHERE id = $1`, projectID)
	})
	var firstNumber int
	if err := testPool.QueryRow(ctx, `
		UPDATE workspace
		SET issue_counter = GREATEST(issue_counter, (SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)) + 2
		WHERE id = $1
		RETURNING issue_counter - 1
	`, testWorkspaceID).Scan(&firstNumber); err != nil {
		t.Fatalf("reserve issue numbers: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO issue (workspace_id, title, status, priority, creator_type, creator_id, position, number, project_id)
		VALUES ($1, 'parity-a', 'todo', 'none', 'member', $2, 1, $3, $4),
		       ($1, 'parity-b', 'done', 'none', 'member', $2, 2, $3 + 1, $4)
	`, testWorkspaceID, testUserID, firstNumber, projectID); err != nil {
		t.Fatalf("seed issues: %v", err)
	}

	collect := func(kind string) map[string]int64 {
		t.Helper()
		w := httptest.NewRecorder()
		testHandler.ListIssueTableGroups(w, newRequest(http.MethodPost, "/api/issues/table/groups", issueTableGroupsRequest{
			Query: statusCategoryQuery(projectID),
			Group: issueTableGroupSpec{Kind: kind},
			Page:  issueTablePageRequest{Limit: 100},
		}))
		if w.Code != http.StatusOK {
			t.Fatalf("%s groups status = %d: %s", kind, w.Code, w.Body.String())
		}
		var groups issueTableGroupsResponse
		if err := json.NewDecoder(w.Body).Decode(&groups); err != nil {
			t.Fatalf("decode %s groups: %v", kind, err)
		}
		out := map[string]int64{}
		for _, group := range groups.Groups {
			out[group.Value.Status] = group.Count
		}
		return out
	}

	byStatus := collect("status")
	byCategory := collect("status_category")
	if len(byStatus) != len(byCategory) {
		t.Fatalf("group counts differ: status=%#v category=%#v", byStatus, byCategory)
	}
	for status, count := range byStatus {
		if byCategory[status] != count {
			t.Fatalf("group %q: status=%d category=%d", status, count, byCategory[status])
		}
	}
}
