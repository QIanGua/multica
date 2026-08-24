package handler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/seatcapacity"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestSeatCapacityIntentCannotRegressOrBeDeletedByStaleWorker(t *testing.T) {
	ctx := context.Background()
	workspaceID := parseUUID(testWorkspaceID)
	token, linkID, userID, memberID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	queries := db.New(testPool)

	_, err := queries.UpsertSeatCapacityIntent(ctx, db.UpsertSeatCapacityIntentParams{
		WorkspaceID: uuidToPG(uuid.UUID(workspaceID.Bytes)), OperationToken: uuidToPG(token),
		Action: seatcapacity.ActionClaimShareJoin, SubjectID: uuidToPG(token),
		ShareLinkID: uuidToPG(linkID), UserID: uuidToPG(userID),
		NextAttemptAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(ctx, `DELETE FROM seat_capacity_outbox WHERE workspace_id = $1`, workspaceID)
	})

	rows, err := queries.TransitionSeatCapacityIntent(ctx, db.TransitionSeatCapacityIntentParams{
		NextAction: seatcapacity.ActionConfirm, CurrentAction: seatcapacity.ActionClaimShareJoin,
		MemberID: uuidToPG(memberID), OperationToken: uuidToPG(token),
		NextAttemptAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	if err != nil || rows != 1 {
		t.Fatalf("transition rows=%d err=%v", rows, err)
	}

	_, err = queries.UpsertSeatCapacityIntent(ctx, db.UpsertSeatCapacityIntentParams{
		WorkspaceID: uuidToPG(uuid.UUID(workspaceID.Bytes)), OperationToken: uuidToPG(token),
		Action: seatcapacity.ActionClaimShareJoin, SubjectID: uuidToPG(token),
		ShareLinkID: uuidToPG(linkID), UserID: uuidToPG(userID),
		NextAttemptAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("regressive upsert error = %v, want pgx.ErrNoRows", err)
	}
	if err := queries.DeleteSeatCapacityIntentForAction(ctx, db.DeleteSeatCapacityIntentForActionParams{
		OperationToken: uuidToPG(token), Action: seatcapacity.ActionClaimShareJoin,
	}); err != nil {
		t.Fatal(err)
	}
	intent, err := queries.GetSeatCapacityIntent(ctx, uuidToPG(token))
	if err != nil {
		t.Fatal(err)
	}
	if intent.Action != seatcapacity.ActionConfirm || !intent.MemberID.Valid || intent.MemberID.Bytes != memberID {
		t.Fatalf("intent regressed or was deleted: %+v", intent)
	}
	if err := enqueueCapacityRelease(ctx, queries, uuid.UUID(workspaceID.Bytes), token); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("release over confirm error = %v, want pgx.ErrNoRows", err)
	}
	intent, err = queries.GetSeatCapacityIntent(ctx, uuidToPG(token))
	if err != nil {
		t.Fatal(err)
	}
	if intent.Action != seatcapacity.ActionConfirm {
		t.Fatalf("release regressed confirmed intent to %q", intent.Action)
	}

	if err := queries.DeleteSeatCapacityConfirmIntentsForWorkspaceDeletion(ctx, workspaceID); err != nil {
		t.Fatal(err)
	}
	if err := queries.PrepareSeatCapacityOperationReleasesForWorkspaceDeletion(ctx, workspaceID); err != nil {
		t.Fatal(err)
	}
	if err := queries.PrepareSeatCapacityInvitationReleasesForWorkspaceDeletion(ctx, workspaceID); err != nil {
		t.Fatal(err)
	}
	if err := queries.PrepareSeatCapacityMemberReleasesForWorkspaceDeletion(ctx, workspaceID); err != nil {
		t.Fatal(err)
	}
	_, err = queries.GetSeatCapacityIntent(ctx, uuidToPG(token))
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("confirmed token intent survived workspace deletion: %v", err)
	}
	var memberReleases int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM seat_capacity_outbox
		WHERE workspace_id = $1 AND action = 'release_member'
	`, workspaceID).Scan(&memberReleases); err != nil {
		t.Fatal(err)
	}
	if memberReleases == 0 {
		t.Fatal("workspace deletion did not enqueue member capacity releases")
	}
}

func TestSeatCapacityWorkspaceDeletionSettlesOverlappingInvitationIntent(t *testing.T) {
	ctx := context.Background()
	queries := db.New(testPool)
	workspaceID := parseUUID(testWorkspaceID)
	invitationID := uuid.New()
	_, err := testPool.Exec(ctx, `
		INSERT INTO workspace_invitation (
			id, workspace_id, inviter_id, invitee_email, role, status, expires_at
		) VALUES ($1, $2, $3, $4, 'member', 'pending', now() + interval '1 day')
	`, invitationID, workspaceID, parseUUID(testUserID), invitationID.String()+"@multica.ai")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(ctx, `DELETE FROM seat_capacity_outbox WHERE operation_token = $1`, invitationID)
		_, _ = testPool.Exec(ctx, `DELETE FROM workspace_invitation WHERE id = $1`, invitationID)
	})
	_, err = queries.UpsertSeatCapacityIntent(ctx, db.UpsertSeatCapacityIntentParams{
		WorkspaceID: workspaceID, OperationToken: uuidToPG(invitationID),
		Action: seatcapacity.ActionReserveInvitation, SubjectID: uuidToPG(invitationID),
		InvitationID:  uuidToPG(invitationID),
		ExpiresAt:     pgtype.Timestamptz{Time: time.Now().Add(24 * time.Hour), Valid: true},
		NextAttemptAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := queries.PrepareSeatCapacityOperationReleasesForWorkspaceDeletion(ctx, workspaceID); err != nil {
		t.Fatal(err)
	}
	if err := queries.PrepareSeatCapacityInvitationReleasesForWorkspaceDeletion(ctx, workspaceID); err != nil {
		t.Fatal(err)
	}
	intent, err := queries.GetSeatCapacityIntent(ctx, uuidToPG(invitationID))
	if err != nil {
		t.Fatal(err)
	}
	if intent.Action != seatcapacity.ActionRelease || intent.MemberID.Valid || intent.DeadLetteredAt.Valid {
		t.Fatalf("overlapping invitation intent = %+v, want live release", intent)
	}
}

func TestClaimNextDueSeatCapacityIntentIsExclusiveAcrossReplicas(t *testing.T) {
	ctx := context.Background()
	queries := db.New(testPool)
	workspaceID := parseUUID(testWorkspaceID)
	token := uuid.New()
	_, err := queries.UpsertSeatCapacityIntent(ctx, db.UpsertSeatCapacityIntentParams{
		WorkspaceID: workspaceID, OperationToken: uuidToPG(token),
		Action: seatcapacity.ActionConfirm, MemberID: uuidToPG(uuid.New()),
		NextAttemptAt: pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(ctx, `DELETE FROM seat_capacity_outbox WHERE operation_token = $1`, token)
	})

	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, claimErr := queries.ClaimNextDueSeatCapacityIntent(ctx, pgtype.Timestamptz{
				Time: time.Now().Add(5 * time.Minute), Valid: true,
			})
			results <- claimErr
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var claimed, empty int
	for claimErr := range results {
		switch {
		case claimErr == nil:
			claimed++
		case errors.Is(claimErr, pgx.ErrNoRows):
			empty++
		default:
			t.Fatal(claimErr)
		}
	}
	if claimed != 1 || empty != 1 {
		t.Fatalf("claimed=%d empty=%d, want 1/1", claimed, empty)
	}
}
