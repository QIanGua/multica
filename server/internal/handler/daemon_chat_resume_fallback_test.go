package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	obsmetrics "github.com/multica-ai/multica/server/internal/metrics"
	"github.com/prometheus/client_golang/prometheus"
)

func TestChatSessionResumeFallbackNeeded(t *testing.T) {
	tests := []struct {
		name           string
		priorSessionID string
		priorWorkDir   string
		want           bool
	}{
		{name: "both present", priorSessionID: "session", priorWorkDir: "/work", want: false},
		{name: "session missing", priorWorkDir: "/work", want: true},
		{name: "workdir missing", priorSessionID: "session", want: true},
		{name: "both missing", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := chatSessionResumeFallbackNeeded(tt.priorSessionID, tt.priorWorkDir); got != tt.want {
				t.Fatalf("chatSessionResumeFallbackNeeded(%q, %q) = %v, want %v", tt.priorSessionID, tt.priorWorkDir, got, tt.want)
			}
		})
	}
}

func TestClaimTaskChatCompletePointerSkipsSessionFallbackQuery(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	ctx := context.Background()
	agentID, runtimeID, daemonID := createRuntimeGuardAgent(t, ctx)

	var chatSessionID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO chat_session (
			workspace_id, agent_id, creator_id, title,
			session_id, work_dir, runtime_id
		)
		VALUES ($1, $2, $3, 'complete resume pointer', 'pointer-session', '/pointer-workdir', $4)
		RETURNING id
	`, testWorkspaceID, agentID, testUserID, runtimeID).Scan(&chatSessionID); err != nil {
		t.Fatalf("setup: create chat session: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM chat_session WHERE id = $1`, chatSessionID) })

	if _, err := testPool.Exec(ctx, `
		INSERT INTO chat_message (chat_session_id, role, content)
		VALUES ($1, 'user', 'keep the direct pointer')
	`, chatSessionID); err != nil {
		t.Fatalf("setup: create chat input: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, chat_session_id, status, priority)
		VALUES ($1, $2, $3, 'queued', 1000)
	`, agentID, runtimeID, chatSessionID); err != nil {
		t.Fatalf("setup: create chat task: %v", err)
	}

	claimMetrics := obsmetrics.NewBusinessMetrics()
	h := *testHandler
	h.Metrics = claimMetrics

	w := httptest.NewRecorder()
	req := newDaemonTokenRequest(http.MethodPost, "/api/daemon/runtimes/"+runtimeID+"/claim", nil,
		testWorkspaceID, daemonID)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("runtimeId", runtimeID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	h.ClaimTaskByRuntime(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ClaimTaskByRuntime: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Task *claimRuntimeGuardTask `json:"task"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Task == nil {
		t.Fatal("expected a claimed task")
	}
	if resp.Task.PriorSessionID != "pointer-session" || resp.Task.PriorWorkDir != "/pointer-workdir" {
		t.Fatalf("claim pointer = (%q, %q), want direct chat-session pointer", resp.Task.PriorSessionID, resp.Task.PriorWorkDir)
	}

	registry := prometheus.NewRegistry()
	registry.MustRegister(claimMetrics.Collectors()...)
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather claim metrics: %v", err)
	}
	seenRolloutQuery := false
	for _, family := range families {
		switch family.GetName() {
		case "multica_chat_claim_session_fallback_needed_total":
			if len(family.Metric) != 1 || family.Metric[0].GetCounter().GetValue() != 0 {
				t.Fatalf("complete pointer unexpectedly needed session fallback: %v", family)
			}
		case "multica_chat_claim_session_fallback_result_total":
			t.Fatalf("complete pointer unexpectedly emitted a session fallback result: %v", family)
		case "multica_chat_claim_resume_query_duration_seconds":
			for _, metric := range family.Metric {
				for _, label := range metric.Label {
					if label.GetName() != "query" {
						continue
					}
					switch label.GetValue() {
					case "rollout_missing":
						seenRolloutQuery = true
					case "last_session":
						t.Fatal("complete pointer unexpectedly ran GetLastChatTaskSession")
					}
				}
			}
		}
	}
	if !seenRolloutQuery {
		t.Fatal("independent rollout-missing query was not observed")
	}
}
