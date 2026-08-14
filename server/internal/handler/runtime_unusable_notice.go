package handler

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// noteRuntimeUnusable records, on the issue itself, that a trigger was refused
// because the target's agent CLI cannot run on its machine.
//
// The interactive surfaces (composer chip, post-send toast) already carry the
// dispatch reason code back to whoever was typing. Nothing carries it for the
// triggers that arrive without a person watching a response: an issue assigned
// to the agent, or another agent @mentioning it. Those are exactly the cases
// where a refusal with no trace reads as "Multica silently ignored me", which
// is the failure this whole change exists to remove (MUL-6164) — so they get a
// durable comment naming the agent, the cause, and the command that fixes it.
//
// Best-effort by construction: the refusal already happened and is correct
// whether or not this note lands, so a failure here is logged, never returned.
func (h *Handler) noteRuntimeUnusable(ctx context.Context, issue db.Issue, agent db.Agent, verdict service.AgentVerdict) {
	content := service.RuntimeUnusableNotice(agent.Name, verdict)
	// author_type='system', author_id=zero UUID — same shape as the sub-issue
	// completion notice; clients branch on author_type, not the UUID value.
	comment, err := h.Queries.CreateComment(ctx, db.CreateCommentParams{
		IssueID:     issue.ID,
		WorkspaceID: issue.WorkspaceID,
		AuthorType:  "system",
		AuthorID:    pgtype.UUID{Valid: true},
		Content:     content,
		Type:        "system",
		ParentID:    pgtype.UUID{Valid: false},
	})
	if err != nil {
		slog.Warn("runtime unusable notice: create system comment failed",
			"error", err,
			"issue_id", uuidToString(issue.ID),
			"agent_id", uuidToString(agent.ID))
		return
	}
	h.publish(protocol.EventCommentCreated, uuidToString(issue.WorkspaceID), "system", "", map[string]any{
		"comment":             commentToResponse(comment, nil, nil),
		"issue_title":         issue.Title,
		"issue_assignee_type": textToPtr(issue.AssigneeType),
		"issue_assignee_id":   uuidToPtr(issue.AssigneeID),
		"issue_status":        issue.Status,
	})
}
