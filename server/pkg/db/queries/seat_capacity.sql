-- name: UpsertSeatCapacityIntent :one
INSERT INTO seat_capacity_outbox (
    workspace_id, operation_token, action, subject_id, member_id,
    invitation_id, share_link_id, user_id, expires_at, next_attempt_at
) VALUES (
    sqlc.arg('workspace_id'), sqlc.arg('operation_token'), sqlc.arg('action'),
    sqlc.narg('subject_id'), sqlc.narg('member_id'), sqlc.narg('invitation_id'),
    sqlc.narg('share_link_id'), sqlc.narg('user_id'), sqlc.narg('expires_at'),
    sqlc.arg('next_attempt_at')
)
ON CONFLICT (operation_token) DO UPDATE SET
    action = EXCLUDED.action,
    subject_id = EXCLUDED.subject_id,
    member_id = EXCLUDED.member_id,
    invitation_id = EXCLUDED.invitation_id,
    share_link_id = EXCLUDED.share_link_id,
    user_id = EXCLUDED.user_id,
    expires_at = EXCLUDED.expires_at,
    delivered_at = CASE
        WHEN seat_capacity_outbox.action = EXCLUDED.action THEN seat_capacity_outbox.delivered_at
        ELSE NULL
    END,
    next_attempt_at = EXCLUDED.next_attempt_at,
    last_error = NULL,
    updated_at = now()
WHERE seat_capacity_outbox.action = EXCLUDED.action
   OR (seat_capacity_outbox.action = 'reserve_invitation' AND EXCLUDED.action = 'consume_invitation')
   OR EXCLUDED.action = 'release'
RETURNING *;

-- name: GetSeatCapacityIntent :one
SELECT * FROM seat_capacity_outbox WHERE operation_token = $1;

-- name: GetPendingShareJoinCapacityIntent :one
SELECT * FROM seat_capacity_outbox
WHERE workspace_id = $1
  AND share_link_id = $2
  AND user_id = $3
  AND action = 'claim_share_join'
ORDER BY created_at DESC
LIMIT 1;

-- name: GetMemberReleaseCapacityIntent :one
SELECT * FROM seat_capacity_outbox
WHERE workspace_id = $1
  AND member_id = $2
  AND action = 'release_member'
ORDER BY created_at DESC
LIMIT 1;

-- name: PrepareSeatCapacityWorkspaceDeletion :exec
-- Workspace teardown commits these compensations atomically with removal of
-- the product rows. The FK-free outbox intentionally survives so Cloud can be
-- reconciled after the local workspace no longer exists.
WITH released_operations AS (
    UPDATE seat_capacity_outbox
    SET action = 'release',
        member_id = NULL,
        delivered_at = NULL,
        next_attempt_at = now(),
        last_error = NULL,
        updated_at = now()
    WHERE workspace_id = sqlc.arg('workspace_id')
      AND action <> 'release_member'
),
released_invitations AS (
    INSERT INTO seat_capacity_outbox (
        workspace_id, operation_token, action, subject_id, invitation_id,
        next_attempt_at
    )
    SELECT workspace_id, id, 'release', id, id, now()
    FROM workspace_invitation
    WHERE workspace_id = sqlc.arg('workspace_id')
      AND status = 'pending'
    ON CONFLICT (operation_token) DO UPDATE SET
        action = 'release',
        member_id = NULL,
        delivered_at = NULL,
        next_attempt_at = now(),
        last_error = NULL,
        updated_at = now()
)
INSERT INTO seat_capacity_outbox (
    workspace_id, operation_token, action, member_id, next_attempt_at
)
SELECT m.workspace_id, gen_random_uuid(), 'release_member', m.id, now()
FROM member AS m
WHERE m.workspace_id = sqlc.arg('workspace_id')
  AND NOT EXISTS (
      SELECT 1
      FROM seat_capacity_outbox AS existing
      WHERE existing.workspace_id = m.workspace_id
        AND existing.member_id = m.id
        AND existing.action = 'release_member'
  );

-- name: ListDueSeatCapacityIntents :many
SELECT * FROM seat_capacity_outbox
WHERE next_attempt_at <= now()
ORDER BY next_attempt_at, created_at
LIMIT sqlc.arg('row_limit');

-- name: MarkSeatCapacityIntentDelivered :exec
UPDATE seat_capacity_outbox
SET delivered_at = now(),
    attempt_count = attempt_count + 1,
    last_error = NULL,
    updated_at = now()
WHERE operation_token = $1 AND action = $2;

-- name: MarkSeatCapacityIntentFailed :exec
UPDATE seat_capacity_outbox
SET attempt_count = attempt_count + 1,
    last_error = left(sqlc.arg('last_error'), 1000),
    next_attempt_at = sqlc.arg('next_attempt_at'),
    updated_at = now()
WHERE operation_token = sqlc.arg('operation_token') AND action = sqlc.arg('action');

-- name: TransitionSeatCapacityIntent :execrows
UPDATE seat_capacity_outbox
SET action = sqlc.arg('next_action'),
    member_id = sqlc.narg('member_id'),
    delivered_at = NULL,
    next_attempt_at = sqlc.arg('next_attempt_at'),
    last_error = NULL,
    updated_at = now()
WHERE operation_token = sqlc.arg('operation_token')
  AND action = sqlc.arg('current_action');

-- name: DeleteSeatCapacityIntentForAction :exec
DELETE FROM seat_capacity_outbox
WHERE operation_token = $1 AND action = $2;

-- name: ExpireInvitationForCapacityRecovery :exec
UPDATE workspace_invitation
SET status = 'expired', updated_at = now()
WHERE id = $1 AND status = 'pending';
