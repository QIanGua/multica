package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/dispatch"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestAgentReadinessRuntimeAccess is the admission half of MUL-6704, and the
// reason the revoke teardown is not enough on its own.
//
// #7571 made a reclaimed private runtime refuse to CLAIM another owner's agent.
// Admission did not know that: it only asked "is a runtime bound, and is it
// online". So every new trigger for such an agent still enqueued happily, the
// claim fence then refused it from both sides, and two hours later the queue TTL
// failed it as `queued_expired` — "task expired in queue", pointing the user at a
// backlog instead of at the permission that had been taken away.
//
// The agents that hit this are exactly the ones the teardown deliberately leaves
// bound (the `kind = 'system'` builder carriers), so the two halves have to ship
// together.
func TestAgentReadinessRuntimeAccess(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name       string
		visibility string
		sameOwner  bool
		wantReady  bool
		wantReason dispatch.ReasonCode
	}{
		{
			name:       "public runtime admits a foreign agent",
			visibility: "public",
			wantReady:  true,
		},
		{
			name:       "private runtime admits its owner's agent",
			visibility: "private",
			sameOwner:  true,
			wantReady:  true,
		},
		{
			name:       "reclaimed private runtime blocks a foreign agent",
			visibility: "private",
			wantReady:  false,
			wantReason: dispatch.ReasonRuntimeAccessRevoked,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := newResolveOriginatorPool(t)
			bootstrap := testutil.New(pool, "", "")
			suffix := time.Now().UnixNano()

			runtimeOwnerID := bootstrap.User(t,
				fmt.Sprintf("readiness-runtime-owner-%d", suffix),
				fmt.Sprintf("readiness-runtime-owner-%d@example.com", suffix),
			)
			agentOwnerID := runtimeOwnerID
			if !tt.sameOwner {
				agentOwnerID = bootstrap.User(t,
					fmt.Sprintf("readiness-agent-owner-%d", suffix),
					fmt.Sprintf("readiness-agent-owner-%d@example.com", suffix),
				)
			}
			workspaceID := bootstrap.Workspace(t,
				fmt.Sprintf("readiness-access-%d", suffix),
				fmt.Sprintf("readiness-access-%d", suffix),
			)
			fx := testutil.New(pool, workspaceID, runtimeOwnerID)
			fx.Member(t, workspaceID, runtimeOwnerID, "owner")
			if agentOwnerID != runtimeOwnerID {
				fx.Member(t, workspaceID, agentOwnerID, "member")
			}
			runtimeID := fx.Runtime(t, "readiness-runtime", testutil.Cols{
				"visibility": tt.visibility,
				"owner_id":   runtimeOwnerID,
				"status":     "online",
			})
			agentID := fx.Agent(t, "readiness-agent", runtimeID, testutil.Cols{
				"owner_id": agentOwnerID,
			})

			q := db.New(pool)
			agent, err := q.GetAgent(ctx, util.MustParseUUID(agentID))
			if err != nil {
				t.Fatalf("load agent: %v", err)
			}
			verdict, err := AgentReadiness(ctx, q, agent)
			if err != nil {
				t.Fatalf("AgentReadiness: %v", err)
			}
			if verdict.Ready() != tt.wantReady {
				t.Fatalf("Ready() = %v (reason %q), want %v", verdict.Ready(), verdict.Reason, tt.wantReady)
			}
			if tt.wantReady {
				return
			}
			// Blocked, not waitable: waiting for an online machine to allow you
			// is not a plan, and the caller must refuse the trigger instead of
			// queueing it.
			if !verdict.Blocked() {
				t.Fatalf("verdict must be BLOCKED so callers refuse instead of queueing; got availability %v", verdict.Availability)
			}
			if verdict.Reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", verdict.Reason, tt.wantReason)
			}
		})
	}
}
