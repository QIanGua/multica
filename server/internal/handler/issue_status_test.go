package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Issue status catalog tests (MUL-6243).

// seedTestCatalog makes the shared test workspace's catalog present. The
// fixture creates its workspace with raw SQL, so it has no catalog rows —
// which is itself the unseeded case covered by TestUnseededWorkspaceStillAccepts.
func seedTestCatalog(t *testing.T) {
	t.Helper()
	if err := issuestatus.Ensure(context.Background(), testHandler.Queries, parseUUID(testWorkspaceID)); err != nil {
		t.Fatalf("seed catalog: %v", err)
	}
}

// createTestCustomStatus inserts a custom status directly and removes it after
// the test, so catalog state cannot leak between tests in the shared workspace.
func createTestCustomStatus(t *testing.T, key, category string) db.IssueStatus {
	t.Helper()
	seedTestCatalog(t)
	entry, err := testHandler.Queries.CreateIssueStatusEntry(context.Background(), db.CreateIssueStatusEntryParams{
		WorkspaceID: parseUUID(testWorkspaceID),
		Key:         key,
		Name:        key,
		Description: "",
		Category:    category,
		Color:       "#123456",
	})
	if err != nil {
		t.Fatalf("create custom status %q: %v", key, err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_status WHERE id = $1`, entry.ID)
	})
	return entry
}

// TestEnsureIsIdempotent covers the rolling-deploy case: two pods can seed the
// same workspace concurrently and the second must be a no-op, not an error.
func TestEnsureIsIdempotent(t *testing.T) {
	ctx := context.Background()
	seedTestCatalog(t)
	if err := issuestatus.Ensure(ctx, testHandler.Queries, parseUUID(testWorkspaceID)); err != nil {
		t.Fatalf("second Ensure should be a no-op, got: %v", err)
	}

	entries, err := testHandler.Queries.ListIssueStatusEntries(ctx, db.ListIssueStatusEntriesParams{
		WorkspaceID: parseUUID(testWorkspaceID),
	})
	if err != nil {
		t.Fatalf("list catalog: %v", err)
	}

	systemByKey := map[string]db.IssueStatus{}
	for _, e := range entries {
		if e.IsSystem {
			systemByKey[e.Key] = e
		}
	}
	if len(systemByKey) != 7 {
		t.Fatalf("expected exactly 7 built-in rows after double seeding, got %d", len(systemByKey))
	}
	// Every built-in must be its own category's canonical — the invariant that
	// makes Effective an identity function on built-in keys.
	for key, entry := range systemByKey {
		if entry.Category != key {
			t.Errorf("built-in %q has category %q; a built-in must be its own category's canonical", key, entry.Category)
		}
	}
}

// TestCatalogOrderMatchesHistoricalStatusOrder pins the default board order.
// A workspace with no custom statuses must list exactly as it did before this
// feature, or every existing user's board silently rearranges.
func TestCatalogOrderMatchesHistoricalStatusOrder(t *testing.T) {
	seedTestCatalog(t)
	entries, err := testHandler.Queries.ListIssueStatusEntries(context.Background(), db.ListIssueStatusEntriesParams{
		WorkspaceID: parseUUID(testWorkspaceID),
	})
	if err != nil {
		t.Fatalf("list catalog: %v", err)
	}

	var gotSystem []string
	for _, e := range entries {
		if e.IsSystem {
			gotSystem = append(gotSystem, e.Key)
		}
	}
	want := []string{"backlog", "todo", "in_progress", "in_review", "done", "blocked", "cancelled"}
	for i := range want {
		if i >= len(gotSystem) || gotSystem[i] != want[i] {
			t.Fatalf("built-in order = %v, want %v (frontend STATUS_ORDER)", gotSystem, want)
		}
	}
}

// TestUnseededWorkspaceStillAcceptsBuiltInStatuses is the regression guard for
// the failure mode found while wiring this up: requiring a catalog row to
// validate a status made every issue write fail in a workspace whose seed had
// not landed. The built-ins must resolve with or without a row.
func TestUnseededWorkspaceStillAcceptsBuiltInStatuses(t *testing.T) {
	ctx := context.Background()
	// Strip the catalog to simulate a workspace created by a pod that predates
	// the feature, or one the seed migration has not reached yet.
	if _, err := testPool.Exec(ctx, `DELETE FROM issue_status WHERE workspace_id = $1`, parseUUID(testWorkspaceID)); err != nil {
		t.Fatalf("clear catalog: %v", err)
	}
	t.Cleanup(func() { seedTestCatalog(t) })

	for _, key := range issuestatus.Canonical() {
		if _, err := issuestatus.Resolve(ctx, testHandler.Queries, parseUUID(testWorkspaceID), key); err != nil {
			t.Errorf("built-in %q must resolve in an unseeded workspace, got: %v", key, err)
		}
		if got := issuestatus.Effective(ctx, testHandler.Queries, parseUUID(testWorkspaceID), key); got != key {
			t.Errorf("Effective(%q) = %q in an unseeded workspace, want the key unchanged", key, got)
		}
	}

	// A non-built-in key still needs a catalog row, so failing open is scoped
	// exactly to the set that was valid before this feature.
	if _, err := issuestatus.Resolve(ctx, testHandler.Queries, parseUUID(testWorkspaceID), "human_review"); err == nil {
		t.Error("a custom key with no catalog row must not resolve")
	}

	// The error message must never omit a key that Resolve accepts.
	keys, err := issuestatus.ActiveKeys(ctx, testHandler.Queries, parseUUID(testWorkspaceID))
	if err != nil {
		t.Fatalf("ActiveKeys: %v", err)
	}
	if len(keys) != 7 {
		t.Errorf("ActiveKeys on an unseeded workspace = %v, want the 7 built-ins", keys)
	}
}

// TestCustomStatusInheritsItsCategoryBehavior is the core promise of the
// model: a custom status behaves as the canonical status it names.
func TestCustomStatusInheritsItsCategoryBehavior(t *testing.T) {
	ctx := context.Background()
	cases := []struct{ key, category string }{
		{"human_review_t", issuestatus.InReview},
		{"rework_t", issuestatus.Todo},
		{"gate_approved_t", issuestatus.Done},
		{"waiting_customer_t", issuestatus.Blocked},
		{"triage_later_t", issuestatus.Backlog},
	}
	for _, tc := range cases {
		createTestCustomStatus(t, tc.key, tc.category)
		got := issuestatus.Effective(ctx, testHandler.Queries, parseUUID(testWorkspaceID), tc.key)
		if got != tc.category {
			t.Errorf("Effective(%q) = %q, want %q", tc.key, got, tc.category)
		}
	}
}

// TestIssueWriteAcceptsCustomStatusAndRejectsUnknown exercises the real HTTP
// write path, which is where the dropped enum CHECK has to be replaced.
func TestIssueWriteAcceptsCustomStatusAndRejectsUnknown(t *testing.T) {
	createTestCustomStatus(t, "human_review_w", issuestatus.InReview)

	t.Run("custom status is accepted", func(t *testing.T) {
		req := newRequest(http.MethodPost, "/api/issues", map[string]any{
			"title":  "custom status write",
			"status": "human_review_w",
		})
		rec := httptest.NewRecorder()
		testHandler.CreateIssue(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
		}
		var created struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if created.Status != "human_review_w" {
			t.Errorf("issue.status = %q, want the custom key stored verbatim", created.Status)
		}
		t.Cleanup(func() {
			testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, parseUUID(created.ID))
		})
	})

	t.Run("unknown status is rejected", func(t *testing.T) {
		req := newRequest(http.MethodPost, "/api/issues", map[string]any{
			"title":  "unknown status write",
			"status": "not_a_status",
		})
		rec := httptest.NewRecorder()
		testHandler.CreateIssue(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for an unknown status, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("archived status is rejected", func(t *testing.T) {
		entry := createTestCustomStatus(t, "retired_w", issuestatus.InProgress)
		if _, err := testHandler.Queries.ArchiveIssueStatusEntry(context.Background(), db.ArchiveIssueStatusEntryParams{
			ID:          entry.ID,
			WorkspaceID: parseUUID(testWorkspaceID),
		}); err != nil {
			t.Fatalf("archive: %v", err)
		}
		req := newRequest(http.MethodPost, "/api/issues", map[string]any{
			"title":  "archived status write",
			"status": "retired_w",
		})
		rec := httptest.NewRecorder()
		testHandler.CreateIssue(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for an archived status, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}

// TestBuiltInStatusesAreImmutable pins the product decision that built-in
// statuses cannot be renamed, recolored, or archived, so the default workspace
// is identical for every user who never opens the settings page.
func TestBuiltInStatusesAreImmutable(t *testing.T) {
	ctx := context.Background()
	seedTestCatalog(t)
	builtIn, err := testHandler.Queries.GetIssueStatusEntryByKey(ctx, db.GetIssueStatusEntryByKeyParams{
		WorkspaceID: parseUUID(testWorkspaceID),
		Key:         "in_review",
	})
	if err != nil {
		t.Fatalf("load built-in: %v", err)
	}

	t.Run("rename is refused at the API", func(t *testing.T) {
		req := withURLParam(
			newRequest(http.MethodPatch, "/api/issue-statuses/"+uuidToString(builtIn.ID), map[string]any{"name": "Renamed"}),
			"id", uuidToString(builtIn.ID))
		rec := httptest.NewRecorder()
		testHandler.UpdateIssueStatus(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403 renaming a built-in, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("archive is refused at the API", func(t *testing.T) {
		req := withURLParam(
			newRequest(http.MethodDelete, "/api/issue-statuses/"+uuidToString(builtIn.ID), nil),
			"id", uuidToString(builtIn.ID))
		rec := httptest.NewRecorder()
		testHandler.ArchiveIssueStatus(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403 archiving a built-in, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	// Defense in depth: even a direct query cannot rename or archive a
	// built-in, because the statements carry an is_system guard.
	t.Run("storage layer refuses too", func(t *testing.T) {
		if _, err := testHandler.Queries.UpdateIssueStatusEntry(ctx, db.UpdateIssueStatusEntryParams{
			ID:          builtIn.ID,
			WorkspaceID: parseUUID(testWorkspaceID),
			Name:        pgtype.Text{String: "Renamed", Valid: true},
		}); err == nil {
			t.Error("UpdateIssueStatusEntry must not touch a built-in row")
		}
		if _, err := testHandler.Queries.ArchiveIssueStatusEntry(ctx, db.ArchiveIssueStatusEntryParams{
			ID:          builtIn.ID,
			WorkspaceID: parseUUID(testWorkspaceID),
		}); err == nil {
			t.Error("ArchiveIssueStatusEntry must not touch a built-in row")
		}
	})
}

// TestArchiveRefusesWhileIssuesStillUseTheStatus is the decision recorded on
// the issue: migrate the issues first rather than silently rewriting them.
func TestArchiveRefusesWhileIssuesStillUseTheStatus(t *testing.T) {
	ctx := context.Background()
	entry := createTestCustomStatus(t, "in_use_a", issuestatus.InProgress)

	req := newRequest(http.MethodPost, "/api/issues", map[string]any{
		"title":  "occupies the status",
		"status": "in_use_a",
	})
	rec := httptest.NewRecorder()
	testHandler.CreateIssue(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create issue: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &created)
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, parseUUID(created.ID))
	})

	archiveReq := withURLParam(
		newRequest(http.MethodDelete, "/api/issue-statuses/"+uuidToString(entry.ID), nil),
		"id", uuidToString(entry.ID))
	archiveRec := httptest.NewRecorder()
	testHandler.ArchiveIssueStatus(archiveRec, archiveReq)
	if archiveRec.Code != http.StatusConflict {
		t.Fatalf("expected 409 while the status is in use, got %d: %s", archiveRec.Code, archiveRec.Body.String())
	}

	// After moving the issue off it, archiving succeeds.
	if _, err := testPool.Exec(ctx, `UPDATE issue SET status = 'todo' WHERE id = $1`, parseUUID(created.ID)); err != nil {
		t.Fatalf("move issue: %v", err)
	}
	retryRec := httptest.NewRecorder()
	testHandler.ArchiveIssueStatus(retryRec, withURLParam(
		newRequest(http.MethodDelete, "/api/issue-statuses/"+uuidToString(entry.ID), nil),
		"id", uuidToString(entry.ID)))
	if retryRec.Code != http.StatusOK {
		t.Fatalf("expected 200 once the status is unused, got %d: %s", retryRec.Code, retryRec.Body.String())
	}
}

// TestCreateIssueStatusValidation covers the reserved-key and category rules.
func TestCreateIssueStatusValidation(t *testing.T) {
	seedTestCatalog(t)

	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"reserved built-in key", map[string]any{"name": "Mine", "key": "in_review", "category": "in_review", "color": "#123456"}, http.StatusBadRequest},
		{"name slugifying onto a built-in", map[string]any{"name": "In Review", "category": "in_review", "color": "#123456"}, http.StatusBadRequest},
		{"unknown category", map[string]any{"name": "Weird", "category": "started", "color": "#123456"}, http.StatusBadRequest},
		{"missing category", map[string]any{"name": "Weird2", "color": "#123456"}, http.StatusBadRequest},
		{"bad color", map[string]any{"name": "Weird3", "category": "todo", "color": "red"}, http.StatusBadRequest},
		{"empty name", map[string]any{"name": "  ", "category": "todo", "color": "#123456"}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			testHandler.CreateIssueStatus(rec, newRequest(http.MethodPost, "/api/issue-statuses", tc.body))
			if rec.Code != tc.want {
				t.Fatalf("expected %d, got %d: %s", tc.want, rec.Code, rec.Body.String())
			}
		})
	}
}
