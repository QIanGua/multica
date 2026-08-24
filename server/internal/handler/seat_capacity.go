package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/seatcapacity"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var (
	errSeatCapacityFull        = errors.New("no purchased member seats are available")
	errSeatCapacityUnavailable = errors.New("seat capacity service is unavailable")
)

func (h *Handler) seatCapacityEnabled() bool {
	return h != nil && h.SeatCapacity != nil && h.SeatCapacity.Enabled()
}

func capacityIntentParams(workspaceID, token uuid.UUID, action string, due time.Time) db.UpsertSeatCapacityIntentParams {
	return db.UpsertSeatCapacityIntentParams{
		WorkspaceID:    uuidToPG(workspaceID),
		OperationToken: uuidToPG(token),
		Action:         action,
		NextAttemptAt:  pgtype.Timestamptz{Time: due, Valid: true},
	}
}

func (h *Handler) reserveInvitationCapacity(ctx context.Context, workspaceID, invitationID uuid.UUID, expiresAt time.Time) error {
	if !h.seatCapacityEnabled() {
		return nil
	}
	params := capacityIntentParams(workspaceID, invitationID, seatcapacity.ActionReserveInvitation, seatcapacity.RecoveryDue(time.Now()).Time)
	params.SubjectID = uuidToPG(invitationID)
	params.InvitationID = uuidToPG(invitationID)
	params.ExpiresAt = pgtype.Timestamptz{Time: expiresAt, Valid: true}
	if _, err := h.Queries.UpsertSeatCapacityIntent(ctx, params); err != nil {
		return fmt.Errorf("record invitation capacity intent: %w", err)
	}
	decision, err := h.SeatCapacity.ReserveInvitation(ctx, workspaceID, invitationID, expiresAt)
	if err != nil {
		h.compensateCapacityIntent(ctx, invitationID)
		return fmt.Errorf("%w: %v", errSeatCapacityUnavailable, err)
	}
	if !decision.Managed {
		return h.deleteCapacityIntentForAction(ctx, invitationID, seatcapacity.ActionReserveInvitation)
	}
	if !decision.Allowed {
		_ = h.deleteCapacityIntentForAction(ctx, invitationID, seatcapacity.ActionReserveInvitation)
		if decision.Reason == "capacity_full" || decision.Reason == "denied" {
			return errSeatCapacityFull
		}
		return fmt.Errorf("%w: reservation rejected in state %s", errSeatCapacityUnavailable, decision.Reason)
	}
	if err := h.Queries.MarkSeatCapacityIntentDelivered(ctx, db.MarkSeatCapacityIntentDeliveredParams{
		OperationToken: uuidToPG(invitationID), Action: seatcapacity.ActionReserveInvitation,
	}); err != nil {
		h.compensateCapacityIntent(ctx, invitationID)
		return fmt.Errorf("record invitation capacity reservation: %w", err)
	}
	return nil
}

func (h *Handler) beginCapacityConsume(ctx context.Context, workspaceID, token, invitationID, userID uuid.UUID) error {
	if !h.seatCapacityEnabled() {
		return nil
	}
	params := capacityIntentParams(workspaceID, token, seatcapacity.ActionConsumeInvitation, seatcapacity.RecoveryDue(time.Now()).Time)
	params.SubjectID = uuidToPG(token)
	params.InvitationID = uuidToPG(invitationID)
	params.UserID = uuidToPG(userID)
	if _, err := h.Queries.UpsertSeatCapacityIntent(ctx, params); err != nil {
		return fmt.Errorf("record invitation consume intent: %w", err)
	}
	decision, err := h.SeatCapacity.Consume(ctx, workspaceID, token)
	if err != nil {
		return fmt.Errorf("%w: %v", errSeatCapacityUnavailable, err)
	}
	if !decision.Managed {
		return h.deleteCapacityIntentForAction(ctx, token, seatcapacity.ActionConsumeInvitation)
	}
	if !decision.Allowed {
		_ = h.deleteCapacityIntentForAction(ctx, token, seatcapacity.ActionConsumeInvitation)
		_ = h.Queries.ExpireInvitationForCapacityRecovery(ctx, uuidToPG(invitationID))
		return errSeatCapacityFull
	}
	return h.Queries.MarkSeatCapacityIntentDelivered(ctx, db.MarkSeatCapacityIntentDeliveredParams{
		OperationToken: uuidToPG(token), Action: seatcapacity.ActionConsumeInvitation,
	})
}

func (h *Handler) beginShareJoinCapacity(ctx context.Context, workspaceID, token, shareLinkID, userID uuid.UUID) error {
	if !h.seatCapacityEnabled() {
		return nil
	}
	params := capacityIntentParams(workspaceID, token, seatcapacity.ActionClaimShareJoin, seatcapacity.RecoveryDue(time.Now()).Time)
	params.SubjectID = uuidToPG(token)
	params.ShareLinkID = uuidToPG(shareLinkID)
	params.UserID = uuidToPG(userID)
	if _, err := h.Queries.UpsertSeatCapacityIntent(ctx, params); err != nil {
		return fmt.Errorf("record share-join capacity intent: %w", err)
	}
	decision, err := h.SeatCapacity.ClaimShareJoin(ctx, workspaceID, token)
	if err != nil {
		return fmt.Errorf("%w: %v", errSeatCapacityUnavailable, err)
	}
	if !decision.Managed {
		return h.deleteCapacityIntentForAction(ctx, token, seatcapacity.ActionClaimShareJoin)
	}
	if !decision.Allowed {
		_ = h.deleteCapacityIntentForAction(ctx, token, seatcapacity.ActionClaimShareJoin)
		return errSeatCapacityFull
	}
	return h.Queries.MarkSeatCapacityIntentDelivered(ctx, db.MarkSeatCapacityIntentDeliveredParams{
		OperationToken: uuidToPG(token), Action: seatcapacity.ActionClaimShareJoin,
	})
}

func transitionCapacityIntentToConfirm(ctx context.Context, q *db.Queries, token, memberID uuid.UUID, currentAction string) error {
	rows, err := q.TransitionSeatCapacityIntent(ctx, db.TransitionSeatCapacityIntentParams{
		NextAction: seatcapacity.ActionConfirm, CurrentAction: currentAction,
		MemberID: uuidToPG(memberID), OperationToken: uuidToPG(token),
		NextAttemptAt: seatcapacity.RetryDue(time.Now()),
	})
	if err != nil {
		return err
	}
	if rows != 1 {
		return errors.New("seat capacity intent changed concurrently")
	}
	return nil
}

func enqueueCapacityRelease(ctx context.Context, q *db.Queries, workspaceID, token uuid.UUID) error {
	params := capacityIntentParams(workspaceID, token, seatcapacity.ActionRelease, time.Now())
	params.SubjectID = uuidToPG(token)
	_, err := q.UpsertSeatCapacityIntent(ctx, params)
	return err
}

func enqueueMemberCapacityRelease(ctx context.Context, q *db.Queries, workspaceID, memberID uuid.UUID) error {
	operationToken := uuid.New()
	params := capacityIntentParams(workspaceID, operationToken, seatcapacity.ActionReleaseMember, time.Now())
	params.MemberID = uuidToPG(memberID)
	_, err := q.UpsertSeatCapacityIntent(ctx, params)
	return err
}

func (h *Handler) confirmCapacityIntent(ctx context.Context, workspaceID, token, memberID uuid.UUID) {
	if !h.seatCapacityEnabled() {
		return
	}
	decision, err := h.SeatCapacity.Confirm(ctx, workspaceID, token, memberID)
	if err == nil && (!decision.Managed || decision.Allowed) {
		if deleteErr := h.deleteCapacityIntentForAction(ctx, token, seatcapacity.ActionConfirm); deleteErr == nil {
			return
		} else {
			err = deleteErr
		}
	}
	if err == nil {
		err = fmt.Errorf("confirm rejected in state %s", decision.Reason)
	}
	h.recordCapacityFailure(ctx, token, seatcapacity.ActionConfirm, err)
}

func (h *Handler) compensateCapacityIntent(ctx context.Context, token uuid.UUID) {
	if !h.seatCapacityEnabled() {
		return
	}
	intent, err := h.Queries.GetSeatCapacityIntent(ctx, uuidToPG(token))
	if err != nil {
		return
	}
	// A confirm row belongs to a product transaction that already committed.
	// A losing duplicate request must never release the winning member's seat.
	if intent.Action == seatcapacity.ActionConfirm || intent.Action == seatcapacity.ActionReleaseMember {
		return
	}
	rows, err := h.Queries.TransitionSeatCapacityIntent(ctx, db.TransitionSeatCapacityIntentParams{
		NextAction: seatcapacity.ActionRelease, CurrentAction: intent.Action,
		OperationToken: uuidToPG(token), NextAttemptAt: seatcapacity.RetryDue(time.Now()),
	})
	if err != nil || rows != 1 {
		return
	}
	decision, releaseErr := h.SeatCapacity.Release(ctx, uuid.UUID(intent.WorkspaceID.Bytes), token)
	if (releaseErr == nil && (!decision.Managed || decision.Allowed || decision.Reason == "released" || decision.Reason == "denied")) || seatcapacity.IsNotFound(releaseErr) {
		_ = h.deleteCapacityIntentForAction(ctx, token, seatcapacity.ActionRelease)
		return
	}
	if releaseErr == nil {
		releaseErr = fmt.Errorf("release rejected in state %s", decision.Reason)
	}
	h.recordCapacityFailure(ctx, token, seatcapacity.ActionRelease, releaseErr)
}

func (h *Handler) settleMemberCapacityRelease(ctx context.Context, workspaceID, memberID uuid.UUID) {
	if !h.seatCapacityEnabled() {
		return
	}
	intent, err := h.Queries.GetMemberReleaseCapacityIntent(ctx, db.GetMemberReleaseCapacityIntentParams{
		WorkspaceID: uuidToPG(workspaceID), MemberID: uuidToPG(memberID),
	})
	if err != nil {
		return
	}
	decision, releaseErr := h.SeatCapacity.ReleaseMember(ctx, workspaceID, memberID)
	if (releaseErr == nil && (!decision.Managed || decision.Allowed || decision.Reason == "released")) || seatcapacity.IsNotFound(releaseErr) {
		_ = h.deleteCapacityIntentForAction(ctx, uuid.UUID(intent.OperationToken.Bytes), seatcapacity.ActionReleaseMember)
		return
	}
	if releaseErr == nil {
		releaseErr = fmt.Errorf("member release rejected in state %s", decision.Reason)
	}
	h.recordCapacityFailure(ctx, uuid.UUID(intent.OperationToken.Bytes), intent.Action, releaseErr)
}

func (h *Handler) deleteCapacityIntentForAction(ctx context.Context, token uuid.UUID, action string) error {
	return h.Queries.DeleteSeatCapacityIntentForAction(ctx, db.DeleteSeatCapacityIntentForActionParams{
		OperationToken: uuidToPG(token), Action: action,
	})
}

func (h *Handler) recordCapacityFailure(ctx context.Context, token uuid.UUID, action string, capacityErr error) {
	if capacityErr == nil {
		return
	}
	_ = h.Queries.MarkSeatCapacityIntentFailed(ctx, db.MarkSeatCapacityIntentFailedParams{
		LastError: capacityErr.Error(), NextAttemptAt: pgtype.Timestamptz{Time: time.Now().Add(5 * time.Second), Valid: true},
		OperationToken: uuidToPG(token), Action: action,
	})
	slog.WarnContext(ctx, "seat capacity intent deferred to outbox",
		"operation_token", token.String(), "action", action, "error", capacityErr)
}

func writeSeatCapacityError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errSeatCapacityFull):
		writeErrorCode(w, http.StatusConflict, "seat_capacity_full", "No purchased member seats are available. Add seats in Billing before adding another member.")
	default:
		writeErrorCode(w, http.StatusServiceUnavailable, "seat_capacity_unavailable", "Member capacity could not be verified. Please try again.")
	}
}

func uuidToPG(value uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: value, Valid: value != uuid.Nil}
}
