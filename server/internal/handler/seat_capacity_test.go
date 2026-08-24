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
	"github.com/multica-ai/multica/server/internal/testutil"
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

func TestCapacitySettlementContinuesForInvitationWhileEnforcementIsPaused(t *testing.T) {
	ctx := context.Background()
	workspaceID := uuid.UUID(parseUUID(testWorkspaceID).Bytes)
	token, userID := uuid.New(), uuid.New()
	executor := &stubSeatCapacity{}
	useSeatCapacity(t, executor)
	testHandler.SeatCapacityEnforcementEnabled = false
	previousLocker := testHandler.SeatCapacityLocker
	testHandler.SeatCapacityLocker = seatcapacity.NewWorkspaceLocker(testPool)
	t.Cleanup(func() {
		testHandler.SeatCapacityLocker = previousLocker
		_, _ = testPool.Exec(ctx, `DELETE FROM seat_capacity_outbox WHERE operation_token = $1`, token)
	})

	active, err := testHandler.beginCapacityConsume(ctx, workspaceID, token, token, userID)
	if err != nil {
		t.Fatal(err)
	}
	if !active || executor.consumeCalls != 1 {
		t.Fatalf("active=%v consumeCalls=%d, want true/1", active, executor.consumeCalls)
	}
	intent, err := testHandler.Queries.GetSeatCapacityIntent(ctx, uuidToPG(token))
	if err != nil {
		t.Fatal(err)
	}
	if intent.Action != seatcapacity.ActionConsumeInvitation || !intent.DeliveredAt.Valid {
		t.Fatalf("paused-enforcement consume intent = %+v", intent)
	}
}

func TestSeatCapacityWorkspaceDeletionSettlesOverlappingInvitationIntent(t *testing.T) {
	ctx := context.Background()
	queries := db.New(testPool)
	workspaceID := parseUUID(testWorkspaceID)
	invitationID := uuid.MustParse(dbfx.Insert(t, "workspace_invitation", testutil.Cols{
		"workspace_id":  testWorkspaceID,
		"inviter_id":    testUserID,
		"invitee_email": uuid.NewString() + "@multica.ai",
		"role":          "member",
		"status":        "pending",
		"expires_at":    testutil.Raw("now() + interval '1 day'"),
	}))
	t.Cleanup(func() {
		_, _ = testPool.Exec(ctx, `DELETE FROM seat_capacity_outbox WHERE operation_token = $1`, invitationID)
	})
	_, err := queries.UpsertSeatCapacityIntent(ctx, db.UpsertSeatCapacityIntentParams{
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

func TestSeatCapacityWorkspaceDeletionCannotRegressConcurrentConfirm(t *testing.T) {
	ctx := context.Background()
	queries := db.New(testPool)
	workspaceID := parseUUID(testWorkspaceID)
	invitationID := uuid.MustParse(dbfx.Insert(t, "workspace_invitation", testutil.Cols{
		"workspace_id":  testWorkspaceID,
		"inviter_id":    testUserID,
		"invitee_email": uuid.NewString() + "@multica.ai",
		"role":          "member",
		"status":        "pending",
		"expires_at":    testutil.Raw("now() + interval '1 day'"),
	}))
	t.Cleanup(func() {
		_, _ = testPool.Exec(ctx, `DELETE FROM seat_capacity_outbox WHERE operation_token = $1`, invitationID)
	})
	_, err := queries.UpsertSeatCapacityIntent(ctx, db.UpsertSeatCapacityIntentParams{
		WorkspaceID: workspaceID, OperationToken: uuidToPG(invitationID),
		Action: seatcapacity.ActionConfirm, SubjectID: uuidToPG(invitationID),
		InvitationID: uuidToPG(invitationID), MemberID: uuidToPG(uuid.New()),
		NextAttemptAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := queries.PrepareSeatCapacityInvitationReleasesForWorkspaceDeletion(ctx, workspaceID); err != nil {
		t.Fatal(err)
	}
	intent, err := queries.GetSeatCapacityIntent(ctx, uuidToPG(invitationID))
	if err != nil {
		t.Fatal(err)
	}
	if intent.Action != seatcapacity.ActionConfirm || !intent.MemberID.Valid {
		t.Fatalf("workspace deletion regressed concurrent confirm: %+v", intent)
	}
}

func TestShareJoinCapacityIntentReusesDeadLetterAndSerializesConcurrentCreates(t *testing.T) {
	ctx := context.Background()
	queries := db.New(testPool)
	workspaceID := parseUUID(testWorkspaceID)
	shareLinkID, userID := uuid.New(), uuid.New()
	cleanup := func() {
		_, _ = testPool.Exec(ctx, `
			DELETE FROM seat_capacity_outbox
			WHERE workspace_id = $1 AND share_link_id = $2 AND user_id = $3
		`, workspaceID, shareLinkID, userID)
	}
	cleanup()
	t.Cleanup(cleanup)

	create := func(candidate uuid.UUID) (db.SeatCapacityOutbox, error) {
		return queries.CreateOrReactivateShareJoinCapacityIntent(ctx, db.CreateOrReactivateShareJoinCapacityIntentParams{
			WorkspaceID: workspaceID, OperationToken: uuidToPG(candidate),
			ShareLinkID: uuidToPG(shareLinkID), UserID: uuidToPG(userID),
			NextAttemptAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		})
	}

	first, err := create(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `
		UPDATE seat_capacity_outbox
		SET attempt_count = 10, dead_lettered_at = now(), last_error = 'stuck'
		WHERE operation_token = $1
	`, first.OperationToken); err != nil {
		t.Fatal(err)
	}
	second, err := create(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if second.OperationToken != first.OperationToken {
		t.Fatalf("dead-letter retry token=%v, want original %v", second.OperationToken, first.OperationToken)
	}
	if second.DeadLetteredAt.Valid || second.AttemptCount != 0 || second.LastError.Valid {
		t.Fatalf("dead-letter retry was not reactivated: %+v", second)
	}

	cleanup()
	start := make(chan struct{})
	results := make(chan db.SeatCapacityOutbox, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			intent, createErr := create(uuid.New())
			results <- intent
			errs <- createErr
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for createErr := range errs {
		if createErr != nil {
			t.Fatal(createErr)
		}
	}
	var token pgtype.UUID
	for intent := range results {
		if !token.Valid {
			token = intent.OperationToken
		} else if intent.OperationToken != token {
			t.Fatalf("concurrent creates returned different tokens: %v and %v", token, intent.OperationToken)
		}
	}
	var count int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM seat_capacity_outbox
		WHERE workspace_id = $1 AND share_link_id = $2 AND user_id = $3
	`, workspaceID, shareLinkID, userID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("logical share join rows=%d, want 1", count)
	}
}

func TestStaleSeatCapacityWorkerCannotMutateReactivatedIntent(t *testing.T) {
	ctx := context.Background()
	queries := db.New(testPool)
	workspaceID := parseUUID(testWorkspaceID)
	shareLinkID, userID := uuid.New(), uuid.New()
	created, err := queries.CreateOrReactivateShareJoinCapacityIntent(ctx, db.CreateOrReactivateShareJoinCapacityIntentParams{
		WorkspaceID: workspaceID, OperationToken: uuidToPG(uuid.New()),
		ShareLinkID: uuidToPG(shareLinkID), UserID: uuidToPG(userID),
		NextAttemptAt: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(ctx, `DELETE FROM seat_capacity_outbox WHERE operation_token = $1`, created.OperationToken)
	})
	claimed, err := queries.ClaimNextDueSeatCapacityIntent(ctx, pgtype.Timestamptz{Time: time.Now().Add(5 * time.Minute), Valid: true})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.OperationToken != created.OperationToken || !claimed.LeaseToken.Valid {
		t.Fatalf("claimed unexpected intent: %+v", claimed)
	}
	if _, err := queries.CreateOrReactivateShareJoinCapacityIntent(ctx, db.CreateOrReactivateShareJoinCapacityIntentParams{
		WorkspaceID: workspaceID, OperationToken: uuidToPG(uuid.New()),
		ShareLinkID: uuidToPG(shareLinkID), UserID: uuidToPG(userID),
		NextAttemptAt: pgtype.Timestamptz{Time: time.Now().Add(5 * time.Minute), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}

	transitioned, err := queries.TransitionClaimedSeatCapacityIntent(ctx, db.TransitionClaimedSeatCapacityIntentParams{
		NextAction: seatcapacity.ActionRelease, CurrentAction: claimed.Action,
		OperationToken: claimed.OperationToken, LeaseToken: claimed.LeaseToken,
		NextAttemptAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	if err != nil || transitioned != 0 {
		t.Fatalf("stale transition rows=%d err=%v, want 0", transitioned, err)
	}
	failed, err := queries.MarkClaimedSeatCapacityIntentFailed(ctx, db.MarkClaimedSeatCapacityIntentFailedParams{
		LastError: "stale", NextAttemptAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		OperationToken: claimed.OperationToken, Action: claimed.Action, LeaseToken: claimed.LeaseToken,
	})
	if err != nil || failed != 0 {
		t.Fatalf("stale failure rows=%d err=%v, want 0", failed, err)
	}
	deadLettered, err := queries.MarkClaimedSeatCapacityIntentDeadLettered(ctx, db.MarkClaimedSeatCapacityIntentDeadLetteredParams{
		LastError: "stale", OperationToken: claimed.OperationToken,
		Action: claimed.Action, LeaseToken: claimed.LeaseToken,
	})
	if err != nil || deadLettered != 0 {
		t.Fatalf("stale dead letter rows=%d err=%v, want 0", deadLettered, err)
	}
	deleted, err := queries.DeleteClaimedSeatCapacityIntent(ctx, db.DeleteClaimedSeatCapacityIntentParams{
		OperationToken: claimed.OperationToken, Action: claimed.Action, LeaseToken: claimed.LeaseToken,
	})
	if err != nil || deleted != 0 {
		t.Fatalf("stale delete rows=%d err=%v, want 0", deleted, err)
	}
	intent, err := queries.GetSeatCapacityIntent(ctx, created.OperationToken)
	if err != nil {
		t.Fatal(err)
	}
	if intent.Action != seatcapacity.ActionClaimShareJoin || intent.DeadLetteredAt.Valid || intent.LeaseToken.Valid {
		t.Fatalf("reactivated intent was changed by stale worker: %+v", intent)
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
