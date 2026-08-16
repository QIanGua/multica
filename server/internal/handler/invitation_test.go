package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/middleware"
)

const invitationTestEmail = "invitation-test@multica.ai"

type stubInvitationRateLimiter struct {
	allowed    bool
	err        error
	retryAfter time.Duration
	keys       []string
}

func (l *stubInvitationRateLimiter) Allow(ctx context.Context, key string) bool {
	allowed, _ := l.AllowWithError(ctx, key)
	return allowed
}

func (l *stubInvitationRateLimiter) AllowWithError(_ context.Context, key string) (bool, error) {
	l.keys = append(l.keys, key)
	return l.allowed, l.err
}

func (l *stubInvitationRateLimiter) Check(_ context.Context, _ string) bool {
	return l.allowed
}

func (l *stubInvitationRateLimiter) RetryAfter(_ context.Context, _ string) time.Duration {
	return l.retryAfter
}

func useInvitationRateLimiters(t *testing.T, limiters InvitationRateLimiters) {
	t.Helper()
	previous := testHandler.InvitationRateLimiters
	testHandler.InvitationRateLimiters = limiters
	t.Cleanup(func() {
		testHandler.InvitationRateLimiters = previous
	})
}

func clearInvitationsForTestWorkspace(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := testPool.Exec(ctx,
		`DELETE FROM workspace_invitation WHERE workspace_id = $1`,
		parseUUID(testWorkspaceID),
	); err != nil {
		t.Fatalf("clear invitations: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(),
			`DELETE FROM workspace_invitation WHERE workspace_id = $1`,
			parseUUID(testWorkspaceID),
		)
	})
}

// Sanity check: a fresh, live pending invitation must block re-invitation.
func TestCreateInvitation_BlocksWhilePending(t *testing.T) {
	clearInvitationsForTestWorkspace(t)
	actor := &stubInvitationRateLimiter{allowed: true}
	workspace := &stubInvitationRateLimiter{allowed: true}
	recipient := &stubInvitationRateLimiter{allowed: true}
	useInvitationRateLimiters(t, InvitationRateLimiters{Actor: actor, Workspace: workspace, Recipient: recipient})

	req := newRequest("POST", "/api/workspaces/"+testWorkspaceID+"/members", CreateMemberRequest{
		Email: invitationTestEmail,
		Role:  "member",
	})
	req = withURLParam(req, "id", testWorkspaceID)
	w := httptest.NewRecorder()
	testHandler.CreateInvitation(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("first invite: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	req2 := newRequest("POST", "/api/workspaces/"+testWorkspaceID+"/members", CreateMemberRequest{
		Email: invitationTestEmail,
		Role:  "member",
	})
	req2 = withURLParam(req2, "id", testWorkspaceID)
	w2 := httptest.NewRecorder()
	testHandler.CreateInvitation(w2, req2)
	if w2.Code != http.StatusConflict {
		t.Fatalf("second invite: expected 409 while still pending, got %d: %s", w2.Code, w2.Body.String())
	}
	for name, calls := range map[string]int{
		"actor": len(actor.keys), "workspace": len(workspace.keys), "recipient": len(recipient.keys),
	} {
		if calls != 1 {
			t.Errorf("%s limiter calls = %d, want 1; a pending retry must not consume budget", name, calls)
		}
	}
}

// Regression for issue #2055: an expired pending invitation must NOT block a
// new invitation to the same email. The stale row should be flipped to
// 'expired' and a fresh pending row should be created.
func TestCreateInvitation_AllowsAfterExpiry(t *testing.T) {
	clearInvitationsForTestWorkspace(t)
	ctx := context.Background()

	var staleID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO workspace_invitation (
			workspace_id, inviter_id, invitee_email, role, status, created_at, updated_at, expires_at
		)
		VALUES ($1, $2, $3, 'member', 'pending', now() - interval '10 days', now() - interval '10 days', now() - interval '3 days')
		RETURNING id
	`, parseUUID(testWorkspaceID), parseUUID(testUserID), invitationTestEmail).Scan(&staleID); err != nil {
		t.Fatalf("seed expired invitation: %v", err)
	}

	req := newRequest("POST", "/api/workspaces/"+testWorkspaceID+"/members", CreateMemberRequest{
		Email: invitationTestEmail,
		Role:  "member",
	})
	req = withURLParam(req, "id", testWorkspaceID)
	w := httptest.NewRecorder()
	testHandler.CreateInvitation(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("re-invite after expiry: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp InvitationResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ID == "" || resp.ID == staleID {
		t.Fatalf("expected a new invitation row, got id=%q (stale=%q)", resp.ID, staleID)
	}

	var staleStatus string
	if err := testPool.QueryRow(ctx,
		`SELECT status FROM workspace_invitation WHERE id = $1`, staleID,
	).Scan(&staleStatus); err != nil {
		t.Fatalf("read stale row: %v", err)
	}
	if staleStatus != "expired" {
		t.Fatalf("expected stale row to be 'expired', got %q", staleStatus)
	}

	var pendingCount int
	if err := testPool.QueryRow(ctx, `
		SELECT COUNT(*) FROM workspace_invitation
		WHERE workspace_id = $1 AND invitee_email = $2 AND status = 'pending'
	`, parseUUID(testWorkspaceID), invitationTestEmail).Scan(&pendingCount); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pendingCount != 1 {
		t.Fatalf("expected exactly 1 pending invitation after re-invite, got %d", pendingCount)
	}
}

func TestCreateInvitation_RateLimitChecksEveryGateBeforeWrite(t *testing.T) {
	clearInvitationsForTestWorkspace(t)
	actor := &stubInvitationRateLimiter{allowed: false, retryAfter: 10 * time.Minute}
	workspace := &stubInvitationRateLimiter{allowed: true}
	recipient := &stubInvitationRateLimiter{allowed: false, retryAfter: 24 * time.Hour}
	useInvitationRateLimiters(t, InvitationRateLimiters{Actor: actor, Workspace: workspace, Recipient: recipient})

	const rawEmail = "  Invitation-Limited@Multica.AI  "
	req := newRequest(http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/members", CreateMemberRequest{
		Email: rawEmail,
		Role:  "member",
	})
	req = withURLParam(req, "id", testWorkspaceID)
	rec := httptest.NewRecorder()
	testHandler.CreateInvitation(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "86400" {
		t.Errorf("Retry-After = %q, want 86400", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	var body struct {
		Error             string `json:"error"`
		Code              string `json:"code"`
		RetryAfterSeconds int64  `json:"retry_after_seconds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Error != "too many invitation requests" || body.Code != "invitation_rate_limited" || body.RetryAfterSeconds != 86400 {
		t.Errorf("unexpected body: %+v", body)
	}
	if strings.Contains(rec.Body.String(), "actor") || strings.Contains(rec.Body.String(), "workspace") || strings.Contains(rec.Body.String(), "recipient") {
		t.Errorf("response leaked the limiting gate: %s", rec.Body.String())
	}

	normalizedEmail := strings.ToLower(strings.TrimSpace(rawEmail))
	checks := []struct {
		name string
		got  []string
		want string
	}{
		{name: "actor", got: actor.keys, want: testUserID},
		{name: "workspace", got: workspace.keys, want: testWorkspaceID},
		{name: "recipient", got: recipient.keys, want: invitationRecipientKey(normalizedEmail)},
	}
	for _, check := range checks {
		if len(check.got) != 1 || check.got[0] != check.want {
			t.Errorf("%s limiter keys = %v, want [%s]", check.name, check.got, check.want)
		}
	}
	if len(recipient.keys) == 1 && (recipient.keys[0] == normalizedEmail || strings.Contains(recipient.keys[0], "@")) {
		t.Errorf("recipient limiter key contains the email instead of a digest: %q", recipient.keys[0])
	}

	var pendingCount int
	if err := testPool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM workspace_invitation
		WHERE workspace_id = $1 AND invitee_email = $2 AND status = 'pending'
	`, parseUUID(testWorkspaceID), normalizedEmail).Scan(&pendingCount); err != nil {
		t.Fatalf("count pending invitations: %v", err)
	}
	if pendingCount != 0 {
		t.Fatalf("pending invitation count = %d, want 0 after rate limit rejection", pendingCount)
	}
}

func TestCreateInvitation_RateLimiterFailureReturnsServiceUnavailable(t *testing.T) {
	clearInvitationsForTestWorkspace(t)
	actor := &stubInvitationRateLimiter{allowed: false, err: errors.New("redis unavailable")}
	workspace := &stubInvitationRateLimiter{allowed: true}
	recipient := &stubInvitationRateLimiter{allowed: true}
	useInvitationRateLimiters(t, InvitationRateLimiters{Actor: actor, Workspace: workspace, Recipient: recipient})

	req := newRequest(http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/members", CreateMemberRequest{
		Email: "invitation-limiter-error@multica.ai",
		Role:  "member",
	})
	req = withURLParam(req, "id", testWorkspaceID)
	rec := httptest.NewRecorder()
	testHandler.CreateInvitation(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") != "5" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("unexpected retry/cache headers: %v", rec.Header())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["code"] != "invitation_rate_limiter_unavailable" {
		t.Errorf("code = %q, want invitation_rate_limiter_unavailable", body["code"])
	}
	for name, calls := range map[string]int{
		"actor": len(actor.keys), "workspace": len(workspace.keys), "recipient": len(recipient.keys),
	} {
		if calls != 1 {
			t.Errorf("%s limiter calls = %d, want 1 even when another gate errors", name, calls)
		}
	}
	var pendingCount int
	if err := testPool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM workspace_invitation
		WHERE workspace_id = $1 AND invitee_email = 'invitation-limiter-error@multica.ai' AND status = 'pending'
	`, parseUUID(testWorkspaceID)).Scan(&pendingCount); err != nil {
		t.Fatalf("count pending invitations: %v", err)
	}
	if pendingCount != 0 {
		t.Fatalf("pending invitation count = %d, want 0 after limiter backend failure", pendingCount)
	}
}

func TestCreateInvitation_RouteRequiresAdminRole(t *testing.T) {
	clearInvitationsForTestWorkspace(t)
	ctx := context.Background()
	const memberEmail = "invitation-route-member@multica.ai"
	_, _ = testPool.Exec(ctx, `DELETE FROM "user" WHERE email = $1`, memberEmail)
	var memberUserID string
	if err := testPool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ('Invitation Route Member', $1) RETURNING id`, memberEmail).Scan(&memberUserID); err != nil {
		t.Fatalf("create member user: %v", err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'member')`, testWorkspaceID, memberUserID); err != nil {
		t.Fatalf("create member: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, memberUserID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, memberUserID)
	})

	router := chi.NewRouter()
	router.Route("/api/workspaces/{id}", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireWorkspaceRoleFromURL(testHandler.Queries, "id", "owner", "admin"))
			r.Post("/members", testHandler.CreateInvitation)
		})
	})

	call := func(userID, inviteeEmail string) *httptest.ResponseRecorder {
		var body bytes.Buffer
		if err := json.NewEncoder(&body).Encode(CreateMemberRequest{Email: inviteeEmail, Role: "member"}); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/members", &body)
		req.Header.Set("X-User-ID", userID)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	if rec := call(memberUserID, "invitation-route-rejected@multica.ai"); rec.Code != http.StatusForbidden {
		t.Errorf("member status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if rec := call(testUserID, fmt.Sprintf("invitation-route-owner-%s@multica.ai", testWorkspaceID)); rec.Code != http.StatusCreated {
		t.Errorf("owner status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
}
