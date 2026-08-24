package handler

import (
	"context"
	"errors"
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

	if err := queries.PrepareSeatCapacityWorkspaceDeletion(ctx, workspaceID); err != nil {
		t.Fatal(err)
	}
	intent, err = queries.GetSeatCapacityIntent(ctx, uuidToPG(token))
	if err != nil {
		t.Fatal(err)
	}
	if intent.Action != seatcapacity.ActionRelease || intent.MemberID.Valid {
		t.Fatalf("workspace deletion did not compensate the pending operation: %+v", intent)
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
