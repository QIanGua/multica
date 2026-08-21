package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
)

// MUL-6490 / GH #7328 — the authorization chain must survive a cross-issue hop.
//
// The invariant under test (MUL-3963): a run always acts "on behalf of" exactly
// one human U, and every A2A delegation is judged by whether U is on the target's
// allow-list — never by the agent principal doing the asking. The chain's carrier
// is comment.source_task_id: an agent's comment records the run that wrote it, and
// the run that comment wakes inherits that run's originator.
//
// The bug: CreateComment only stamped source_task_id when the authoring run's task
// was on the SAME issue as the comment. An agent coordinating on an issue it just
// created wrote a NULL lineage, so the woken run resolved to unattributed and
// every @mention / assign / sub-issue inside it hit invocation_not_allowed — while
// the identical delegation on the originating issue succeeded.
//
// The fix propagates the chain unconditionally, which is monotonic: a run can only
// carry the human it ALREADY acts for, and each hop re-runs canInvokeAgent against
// that same human. What must stay impossible is SUBSTITUTING a human — falling
// back to the coordinator's owner, or adopting the target issue's originator —
// because that borrows a different person's authority. Both directions are pinned
// below.

// crossIssueChain is the {coordinator, two issues, one running task} shape every
// case here starts from. issueX is where the coordinator's run lives; issueY is
// the issue it coordinates on.
type crossIssueChain struct {
	CoordinatorID string
	IssueX        string
	IssueY        string
	TaskA         string // the coordinator's running task, on issueX
	RuntimeID     string
}

// newCrossIssueChain seeds a coordinator agent owned by ownerUserID with a running
// task on issueX whose originator is originatorUserID (pass nil for an
// unattributed run, e.g. a schedule/webhook autopilot dispatch).
func newCrossIssueChain(t *testing.T, ownerUserID string, originatorUserID any) crossIssueChain {
	t.Helper()
	runtimeID := handlerTestRuntimeID(t)
	coordinator := seedInvocableAgent(t, "MUL-6490 coordinator", ownerUserID, "private")
	issueX := seedBareIssue(t, coordinator)
	issueY := seedBareIssue(t, coordinator)

	var taskA string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, originator_user_id, accountable_user_id)
		VALUES ($1, $2, $3, 'running', 0, $4, $4) RETURNING id
	`, coordinator, runtimeID, issueX, originatorUserID).Scan(&taskA); err != nil {
		t.Fatalf("seed coordinator task: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskA) })

	return crossIssueChain{CoordinatorID: coordinator, IssueX: issueX, IssueY: issueY, TaskA: taskA, RuntimeID: runtimeID}
}

// seedInvocableAgent creates an agent with the given permission mode. Any
// memberTargets make it public_to and allow-list exactly those users, which is the
// minimum-privilege configuration the report ran: "only Bohan may invoke this".
func seedInvocableAgent(t *testing.T, name, ownerUserID, mode string, memberTargets ...string) string {
	t.Helper()
	ctx := context.Background()
	var agentID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent (
			workspace_id, name, description, runtime_mode, runtime_config,
			runtime_id, visibility, permission_mode, max_concurrent_tasks, owner_id,
			instructions, custom_env, custom_args, mcp_config
		)
		VALUES ($1, $2, '', 'cloud', '{}'::jsonb, $3, 'private', $4, 5, $5, '', '{}'::jsonb, '[]'::jsonb, '[]'::jsonb)
		RETURNING id
	`, testWorkspaceID, name, handlerTestRuntimeID(t), mode, ownerUserID).Scan(&agentID); err != nil {
		t.Fatalf("create agent %q: %v", name, err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent WHERE id = $1`, agentID) })

	for _, target := range memberTargets {
		if _, err := testPool.Exec(ctx, `
			INSERT INTO agent_invocation_target (agent_id, target_type, target_id) VALUES ($1, 'member', $2)
		`, agentID, target); err != nil {
			t.Fatalf("allow-list %q for agent %q: %v", target, name, err)
		}
	}
	return agentID
}

// agentComments posts a comment through the real HTTP surface as the agent,
// speaking from taskID — the only way an agent's lineage reaches the comment row.
// A comment-triggered run must reply under its own trigger, so replyUnder carries
// that parent; it is empty for a run that has no trigger comment to answer.
func agentComments(t *testing.T, agentID, taskID, issueID, content, replyUnder string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"content": content}
	if replyUnder != "" {
		body["parent_id"] = replyUnder
	}
	w := httptest.NewRecorder()
	r := newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", body)
	r.Header.Set("X-Agent-ID", agentID)
	r.Header.Set("X-Task-ID", taskID)
	r = withURLParam(r, "id", issueID)
	testHandler.CreateComment(w, r)
	return w
}

// triggerCommentOf returns the comment a task was woken by, which is the only
// parent its replies may use (taskCoversReplyParent).
func triggerCommentOf(t *testing.T, taskID string) string {
	t.Helper()
	var triggerCommentID pgtype.UUID
	if err := testPool.QueryRow(context.Background(),
		`SELECT trigger_comment_id FROM agent_task_queue WHERE id = $1`, taskID).Scan(&triggerCommentID); err != nil {
		t.Fatalf("read trigger_comment_id: %v", err)
	}
	return uuidToString(triggerCommentID)
}

// queuedTaskFor returns the queued task for (issue, agent) and its originator.
// ok is false when the delegation was refused and nothing was enqueued.
func queuedTaskFor(t *testing.T, issueID, agentID string) (taskID string, originator pgtype.UUID, ok bool) {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `
		SELECT id, originator_user_id FROM agent_task_queue
		WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'
	`, issueID, agentID)
	if err != nil {
		t.Fatalf("query queued task: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return "", pgtype.UUID{}, false
	}
	if err := rows.Scan(&taskID, &originator); err != nil {
		t.Fatalf("scan queued task: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID) })
	return taskID, originator, true
}

func commentSourceTaskOf(t *testing.T, commentID string) pgtype.UUID {
	t.Helper()
	var sourceTaskID pgtype.UUID
	if err := testPool.QueryRow(context.Background(),
		`SELECT source_task_id FROM comment WHERE id = $1`, commentID).Scan(&sourceTaskID); err != nil {
		t.Fatalf("read comment source_task_id: %v", err)
	}
	return sourceTaskID
}

// lastCommentOn returns the newest comment id on an issue.
func lastCommentOn(t *testing.T, issueID string) string {
	t.Helper()
	var commentID string
	if err := testPool.QueryRow(context.Background(),
		`SELECT id FROM comment WHERE issue_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1`, issueID).Scan(&commentID); err != nil {
		t.Fatalf("read last comment: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM comment WHERE id = $1`, commentID) })
	return commentID
}

func mention(agentID string) string {
	return "[@Worker](mention://agent/" + agentID + ") please take this"
}

// TestCrossIssueDelegation_OriginatorChainSurvivesTheHop is the reported bug, end
// to end over HTTP: the delegation that works on the coordinator's own issue must
// keep working when the coordinator delegates on another issue, and the woken run
// must carry the SAME human — that inherited originator is what every later
// mention / assign / sub-issue in it is judged by.
func TestCrossIssueDelegation_OriginatorChainSurvivesTheHop(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	// bohan owns the coordinator and is the only member on the worker's allow-list.
	bohan := testUserID
	worker := seedInvocableAgent(t, "MUL-6490 worker", bohan, "public_to", bohan)

	for _, tc := range []struct {
		name  string
		cross bool
	}{
		{"same issue (already worked)", false},
		{"cross issue (the regression)", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chain := newCrossIssueChain(t, bohan, bohan)
			target := chain.IssueX
			if tc.cross {
				target = chain.IssueY
			}

			if w := agentComments(t, chain.CoordinatorID, chain.TaskA, target, mention(worker), ""); w.Code != http.StatusCreated {
				t.Fatalf("CreateComment: got %d, want 201: %s", w.Code, w.Body.String())
			}

			// The comment must record the run that wrote it. This is the carrier;
			// a NULL here is the break the report saw.
			if got := commentSourceTaskOf(t, lastCommentOn(t, target)); uuidToString(got) != chain.TaskA {
				t.Fatalf("comment.source_task_id = %q, want the authoring run %q", uuidToString(got), chain.TaskA)
			}

			taskID, originator, ok := queuedTaskFor(t, target, worker)
			if !ok {
				t.Fatal("the allow-listed worker was not enqueued: the delegation was refused")
			}
			if uuidToString(originator) != bohan {
				t.Fatalf("woken run originator = %q (valid=%v), want %q — the chain lost its human across the hop",
					uuidToString(originator), originator.Valid, bohan)
			}

			// The reported symptom was one hop further out: work INSIDE the woken
			// run. Both gates read the run's own originator, so both recover.
			t.Run("the woken run can delegate onward", func(t *testing.T) {
				if _, err := testPool.Exec(context.Background(),
					`UPDATE agent_task_queue SET status = 'running' WHERE id = $1`, taskID); err != nil {
					t.Fatalf("advance worker task to running: %v", err)
				}
				second := seedInvocableAgent(t, "MUL-6490 second worker", bohan, "public_to", bohan)

				if w := agentComments(t, worker, taskID, target, mention(second), triggerCommentOf(t, taskID)); w.Code != http.StatusCreated {
					t.Fatalf("onward CreateComment: got %d, want 201: %s", w.Code, w.Body.String())
				}
				if _, _, ok := queuedTaskFor(t, target, second); !ok {
					t.Fatal("@mention from inside the woken run was refused (invocation_not_allowed)")
				}

				// The assign gate reads the same originator through a different
				// resolver, so it is asserted separately rather than assumed.
				req := newRequest(http.MethodPatch, "/api/issues/"+target, nil)
				req.Header.Set("X-Agent-ID", worker)
				req.Header.Set("X-Task-ID", taskID)
				status, msg := testHandler.validateAssigneePair(context.Background(), req, testWorkspaceID,
					pgtype.Text{String: "agent", Valid: true}, util.MustParseUUID(second))
				if status != 0 {
					t.Fatalf("assigning from inside the woken run was refused: %d %s", status, msg)
				}
			})
		})
	}
}

// TestCrossIssueDelegation_NeverSubstitutesAHuman pins the other direction. The
// chain may be CARRIED across an issue boundary; it may never be REPLACED. Alice
// triggers a coordinator that Bohan owns, so an owner fallback (the fix the report
// proposed) would silently upgrade Alice's flow to Bohan's authority.
func TestCrossIssueDelegation_NeverSubstitutesAHuman(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	bohan := testUserID
	_, _, alice := privateAgentTestFixture(t)

	// firstHop admits both members, so every case reaches the second hop and the
	// difference isolates to whose authority the chain carries. secondHop admits
	// ONLY bohan — it is the agent an owner fallback would wrongly unlock.
	firstHop := seedInvocableAgent(t, "MUL-6490 shared worker", bohan, "public_to", bohan, alice)
	secondHop := seedInvocableAgent(t, "MUL-6490 bohan-only worker", bohan, "public_to", bohan)

	for _, tc := range []struct {
		name          string
		originator    any
		wantSecondHop bool
	}{
		{"bohan's chain reaches a bohan-only agent", bohan, true},
		{"alice's chain does not become bohan's", alice, false},
		{"an unattributed chain grants nothing", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The coordinator is owned by BOHAN in every case: only the human at the
			// top of the chain may vary the outcome.
			chain := newCrossIssueChain(t, bohan, tc.originator)

			if w := agentComments(t, chain.CoordinatorID, chain.TaskA, chain.IssueY, mention(firstHop), ""); w.Code != http.StatusCreated {
				t.Fatalf("CreateComment: got %d, want 201: %s", w.Code, w.Body.String())
			}
			firstTaskID, originator, ok := queuedTaskFor(t, chain.IssueY, firstHop)
			if tc.originator == nil {
				// A member-scoped allow-list has no human to match, so the chain
				// stops here. That is the correct fail-closed outcome, not the bug.
				if ok {
					t.Fatal("an unattributed chain must not satisfy a member-scoped allow-list")
				}
				return
			}
			if !ok {
				t.Fatal("first hop was refused although both members are allow-listed")
			}
			if uuidToString(originator) != tc.originator.(string) {
				t.Fatalf("first hop originator = %q, want %q", uuidToString(originator), tc.originator)
			}

			if _, err := testPool.Exec(context.Background(),
				`UPDATE agent_task_queue SET status = 'running' WHERE id = $1`, firstTaskID); err != nil {
				t.Fatalf("advance first hop to running: %v", err)
			}
			if w := agentComments(t, firstHop, firstTaskID, chain.IssueY, mention(secondHop), triggerCommentOf(t, firstTaskID)); w.Code != http.StatusCreated {
				t.Fatalf("second-hop CreateComment: got %d, want 201: %s", w.Code, w.Body.String())
			}
			if _, _, ok := queuedTaskFor(t, chain.IssueY, secondHop); ok != tc.wantSecondHop {
				if tc.wantSecondHop {
					t.Fatal("bohan's own chain was refused a bohan-only agent")
				}
				t.Fatal("a chain acting for alice reached a bohan-only agent: a human was substituted")
			}
		})
	}
}

// TestCrossIssueDelegation_GateAndStampAgreeOnTheHuman closes the split-brain the
// bug created: the invoke gate resolved the human from the request's X-Task-ID
// (cross-issue OK) while the enqueued run resolved it from the stored comment
// (cross-issue NULL). The gate said yes and the run started with no authority, so
// the failure only surfaced one hop later. Both resolvers must answer identically
// for the same action.
func TestCrossIssueDelegation_GateAndStampAgreeOnTheHuman(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	bohan := testUserID
	worker := seedInvocableAgent(t, "MUL-6490 agreement worker", bohan, "public_to", bohan)
	chain := newCrossIssueChain(t, bohan, bohan)

	if w := agentComments(t, chain.CoordinatorID, chain.TaskA, chain.IssueY, mention(worker), ""); w.Code != http.StatusCreated {
		t.Fatalf("CreateComment: got %d, want 201: %s", w.Code, w.Body.String())
	}
	commentID := lastCommentOn(t, chain.IssueY)

	// What the gate saw at admission time, from the request header.
	gateReq := newRequest(http.MethodPost, "/api/issues/"+chain.IssueY+"/comments", nil)
	gateReq.Header.Set("X-Agent-ID", chain.CoordinatorID)
	gateReq.Header.Set("X-Task-ID", chain.TaskA)
	fromGate := testHandler.invokeOriginatorFromRequest(gateReq, "agent", chain.CoordinatorID)

	// What the enqueue path sees afterwards, from the persisted comment.
	fromStamp := uuidToString(testHandler.TaskService.ResolveOriginatorFromTriggerComment(
		ctx, util.MustParseUUID(testWorkspaceID), util.MustParseUUID(commentID)))

	if fromGate != fromStamp {
		t.Fatalf("gate resolved %q but the stored chain resolves %q — a run would start with authority the gate never granted",
			fromGate, fromStamp)
	}
	if fromGate != bohan {
		t.Fatalf("both resolvers agree on %q, want %q", fromGate, bohan)
	}
}
