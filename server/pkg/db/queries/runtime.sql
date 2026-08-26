-- name: ListAgentRuntimes :many
SELECT * FROM agent_runtime
WHERE workspace_id = $1
ORDER BY created_at ASC;

-- name: ListVisibleAgentRuntimes :many
-- A private runtime is another member's machine and must not leak into their
-- runtime list. The owner can always see their own runtime; everyone else
-- sees only runtimes the owner has explicitly shared with the workspace.
SELECT * FROM agent_runtime
WHERE workspace_id = $1
  AND (owner_id = $2 OR visibility = 'public')
ORDER BY created_at ASC;

-- name: GetAgentRuntime :one
SELECT * FROM agent_runtime
WHERE id = $1;

-- name: GetAgentRuntimes :many
-- Batch variant of GetAgentRuntime (MUL-4257): loads every runtime in the
-- input set in one round trip so the machine-level batch claim handler can
-- resolve+authorize all of a daemon's runtimes without one point query per
-- runtime. Rows are returned only for ids that exist; the caller matches them
-- back by id and skips any that are missing.
SELECT * FROM agent_runtime
WHERE id = ANY(@ids::uuid[]);

-- name: LockAgentRuntime :one
-- Acquires a row-level exclusive lock on the runtime row. Used at the
-- top of the cascade-delete transaction so that:
--   1. PostgreSQL's FK validation on agent.runtime_id (FK ... ON DELETE
--      RESTRICT) needs FOR KEY SHARE on the parent runtime row, which
--      conflicts with FOR UPDATE — so any concurrent INSERT or UPDATE
--      that would point a new/moved agent at this runtime blocks until
--      our transaction finishes; and
--   2. concurrent UPDATE/DELETE of the runtime row itself (e.g. another
--      delete attempt) waits for us to commit.
-- Combined with ListUserAgentsByRuntimeForUpdate (which row-locks active and
-- archived user agents) this closes both plan drift and archived-agent restore
-- races under read-committed isolation.
SELECT * FROM agent_runtime
WHERE id = $1
FOR UPDATE;

-- name: LockAgentRuntimeForBind :one
-- MUL-6704: re-read + lock the runtime a caller is about to bind an agent to,
-- from inside that caller's own transaction.
--
-- FOR KEY SHARE is exactly the lock an INSERT/UPDATE of agent.runtime_id takes
-- implicitly for FK validation, so this adds no new contention between binders,
-- but it DOES conflict with the FOR UPDATE that LockAgentRuntime takes in the
-- revoke / delete teardown. That is the point: before this query existed, a
-- binder read the runtime row outside its transaction, blocked on the FK lock
-- while a public → private revoke committed, and then wrote its agent onto the
-- runtime using the stale `public` snapshot — recreating exactly the state the
-- teardown had just cleaned up. Blocking here and re-reading afterwards means
-- the visibility/owner check runs against the committed row.
--
-- Note it must NOT be LockAgentRuntime (FOR UPDATE): that would serialize every
-- agent create on the same machine behind one another.
SELECT * FROM agent_runtime
WHERE id = $1
FOR KEY SHARE;

-- name: GetAgentRuntimeForWorkspace :one
SELECT * FROM agent_runtime
WHERE id = $1 AND workspace_id = $2;

-- name: UpsertAgentRuntime :one
-- (xmax = 0) AS inserted distinguishes a fresh insert (true) from an upsert
-- that updated an existing row (false). Analytics reads this to fire
-- runtime_registered/runtime_ready only on first-time registration.
INSERT INTO agent_runtime (
    workspace_id,
    daemon_id,
    name,
    runtime_mode,
    provider,
    status,
    device_info,
    metadata,
    owner_id,
    last_seen_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())
-- Built-in runtimes carry no profile_id. The arbiter is the partial unique
-- index from migration 121 (WHERE profile_id IS NULL); the predicate must be
-- spelled out so Postgres selects that partial index, not the custom-runtime
-- one on (workspace_id, daemon_id, profile_id).
ON CONFLICT (workspace_id, daemon_id, provider) WHERE profile_id IS NULL
DO UPDATE SET
    name = EXCLUDED.name,
    runtime_mode = EXCLUDED.runtime_mode,
    status = EXCLUDED.status,
    device_info = EXCLUDED.device_info,
    metadata = EXCLUDED.metadata,
    owner_id = COALESCE(EXCLUDED.owner_id, agent_runtime.owner_id),
    last_seen_at = now(),
    updated_at = now()
RETURNING *, (xmax = 0) AS inserted;

-- name: UpsertAgentRuntimeWithProfile :one
-- Custom-runtime registration: a daemon resolved a workspace runtime_profile's
-- command_name on PATH and is registering an instance of it. The arbiter is the
-- partial unique index from migration 120 (WHERE profile_id IS NOT NULL), so a
-- single daemon can host the built-in provider AND any number of custom
-- profiles of the same protocol family. provider stays the protocol family so
-- task routing (agent.New(provider)) is unchanged; profile_id is the stable
-- identity. (xmax = 0) AS inserted mirrors UpsertAgentRuntime.
INSERT INTO agent_runtime (
    workspace_id,
    daemon_id,
    name,
    runtime_mode,
    provider,
    status,
    device_info,
    metadata,
    owner_id,
    profile_id,
    last_seen_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now())
ON CONFLICT (workspace_id, daemon_id, profile_id) WHERE profile_id IS NOT NULL
DO UPDATE SET
    name = EXCLUDED.name,
    runtime_mode = EXCLUDED.runtime_mode,
    provider = EXCLUDED.provider,
    status = EXCLUDED.status,
    device_info = EXCLUDED.device_info,
    metadata = EXCLUDED.metadata,
    owner_id = COALESCE(EXCLUDED.owner_id, agent_runtime.owner_id),
    last_seen_at = now(),
    updated_at = now()
RETURNING *, (xmax = 0) AS inserted;

-- name: UpdateAgentRuntimeVisibility :one
-- Toggles a runtime between 'private' (only owner can bind agents) and
-- 'public' (any workspace member can). Default for new rows is 'private'
-- (see migration 083). Gated at the handler layer to owner / workspace
-- admin only.
UPDATE agent_runtime
SET visibility = @visibility, updated_at = now()
WHERE id = @id
RETURNING *;

-- name: UpdateAgentRuntimeCustomName :one
-- Sets or clears a runtime's user-facing custom name (MUL-4217). custom_name
-- overrides the daemon-proposed `name` for display; passing NULL reverts to
-- the default. Kept separate from the registration upserts above (which do
-- name = EXCLUDED.name on every heartbeat) so a custom name is never
-- clobbered by the daemon. Gated at the handler to owner / workspace admin.
UPDATE agent_runtime
SET custom_name = @custom_name, updated_at = now()
WHERE id = @id
RETURNING *;

-- name: UpdateAgentRuntimeCustomNameByDaemon :many
-- Machine-level rename (MUL-4217): applies one custom name to every runtime
-- sharing a daemon_id in the workspace, since a single machine hosts one
-- runtime per provider. @owner_id is NULL for workspace owners/admins (rename
-- the whole machine) or the actor's user id otherwise (only their own
-- runtimes on that machine), so a member cannot relabel someone else's
-- runtime that happens to share the host.
UPDATE agent_runtime
SET custom_name = @custom_name, updated_at = now()
WHERE workspace_id = @workspace_id
  AND daemon_id = @daemon_id
  AND (@owner_id::uuid IS NULL OR owner_id = @owner_id)
RETURNING *;

-- name: ListDaemonCustomNames :many
-- Lists the custom_name of every OTHER runtime on (workspace_id, daemon_id)
-- (MUL-4217). @exclude_id drops the just-registered row. The caller derives
-- the machine-level name in Go — the same "all runtimes share one non-null
-- name" rule the frontend applies in sharedCustomName — so a freshly-added
-- runtime on an already-named machine can inherit that name and keep the
-- machine's display name stable. A daemon hosts only a handful of runtimes
-- (one per provider), so this is a tiny read.
SELECT custom_name FROM agent_runtime
WHERE workspace_id = @workspace_id
  AND daemon_id = @daemon_id
  AND id <> @exclude_id;


-- name: TouchAgentRuntimeLastSeen :execrows
-- Bumps last_seen_at on an already-online runtime. Deliberately does NOT
-- touch status or updated_at: status is unchanged on the hot heartbeat path,
-- and avoiding updated_at keeps the row HOT-eligible (no index columns
-- change) and avoids invalidating any downstream consumer that watches
-- updated_at.
--
-- The status='online' predicate is load-bearing: callers read rt.Status from
-- a prior SELECT and may race with the sweeper, which can flip the row to
-- offline between that SELECT and this UPDATE. Without the predicate this
-- query would silently leave a freshly-heartbeated runtime stuck in offline.
-- Returning affected rows lets callers detect that race and fall back to
-- MarkAgentRuntimeOnline to flip the row back online.
UPDATE agent_runtime
SET last_seen_at = now()
WHERE id = $1 AND status = 'online';

-- name: TouchAgentRuntimesLastSeenBatch :execrows
-- Bulk variant of TouchAgentRuntimeLastSeen used by the BatchedHeartbeatScheduler:
-- coalesces N per-runtime "bump last_seen_at" requests into a single UPDATE so a
-- fleet beating every 15s costs ~1 DB transaction per batch tick instead of N.
--
-- Same load-bearing predicate as the single-id form: status='online' avoids
-- silently un-deleting a sweeper-flipped offline row, and we deliberately do
-- NOT touch updated_at so the rows stay HOT-eligible. Affected-rows < len(ids)
-- means some IDs raced to offline between Schedule and flush; their next beat
-- will fall through the recordHeartbeat sync path and call MarkAgentRuntimeOnline.
UPDATE agent_runtime
SET last_seen_at = now()
WHERE id = ANY(@ids::uuid[]) AND status = 'online';

-- name: MarkAgentRuntimeOnline :one
-- Used on the offline→online transition (and on first heartbeat after
-- registration). Writes status, last_seen_at, and updated_at because the
-- status flip is a real state change and we want updated_at to reflect it.
UPDATE agent_runtime
SET status = 'online', last_seen_at = now(), updated_at = now()
WHERE id = $1
RETURNING *;

-- name: SetAgentRuntimeOffline :exec
UPDATE agent_runtime
SET status = 'offline', updated_at = now()
WHERE id = $1;

-- name: SetAgentRuntimeOfflineWithReason :exec
-- Takes a runtime offline and records WHY, for the one class of cause the user
-- has to repair before the runtime can come back (MUL-6164). Everything that
-- merely stops — daemon shutdown, laptop asleep — uses SetAgentRuntimeOffline
-- and leaves no reason, because "wait for it" needs no explanation.
--
-- Merged into metadata rather than a column: registration overwrites metadata
-- wholesale (`metadata = EXCLUDED.metadata`), so a runtime that comes back
-- drops the reason as a side effect of being usable again — there is no stale
-- explanation to clean up and no code path that has to remember to clear it.
UPDATE agent_runtime
SET status = 'offline',
    metadata = metadata || jsonb_build_object('offline_reason', @offline_reason::jsonb),
    updated_at = now()
WHERE id = $1;

-- name: SelectStaleOnlineRuntimes :many
-- Lists online runtimes whose last_seen_at exceeds the stale window. The
-- sweeper uses this as a candidate set, then optionally filters via the
-- LivenessStore before flipping rows to offline (a fresh Redis liveness
-- record means the DB row is just lagging, not actually dead).
SELECT id, workspace_id, owner_id, daemon_id, provider FROM agent_runtime
WHERE status = 'online'
  AND last_seen_at < now() - make_interval(secs => @stale_seconds::double precision);

-- name: MarkRuntimesOfflineByIDs :many
-- Flips a known set of runtime IDs from online to offline. Paired with
-- SelectStaleOnlineRuntimes in the sweeper so the candidate selection and
-- the actual write are decoupled (the LivenessStore filter sits between).
--
-- Re-checks the stale predicate inside the UPDATE so a concurrent heartbeat
-- between the SELECT (candidate gather), the LivenessStore filter, and this
-- UPDATE cannot demote a runtime that just refreshed last_seen_at. The
-- legacy MarkStaleRuntimesOffline UPDATE had this property implicitly
-- because the predicate and the write lived in one statement; here we
-- carry it forward explicitly so the SELECT/filter/UPDATE pipeline retains
-- the same race-freedom.
UPDATE agent_runtime
SET status = 'offline', updated_at = now()
WHERE status = 'online'
  AND id = ANY(@ids::uuid[])
  AND last_seen_at < now() - make_interval(secs => @stale_seconds::double precision)
RETURNING id, workspace_id, owner_id, daemon_id, provider;

-- name: FailTasksForOfflineRuntimes :many
-- Marks dispatched/running/waiting_local_directory tasks as failed when
-- their runtime has remained offline beyond the reconnect grace. A short or
-- medium network partition must not terminate a daemon process that is still
-- running locally; a real daemon restart is recovered separately through
-- RecoverOrphanedTasksForRuntime. Bounded per tick so a large recovery backlog
-- cannot monopolise the sweeper transaction. last_seen_at is normally set by
-- the first heartbeat; updated_at is only the fallback for a never-heartbeated
-- runtime, and a forced-offline write starts its grace from that update.
WITH victims AS (
  SELECT task.id
  FROM agent_task_queue task
  JOIN agent_runtime runtime ON runtime.id = task.runtime_id
  WHERE task.status IN ('dispatched', 'running', 'waiting_local_directory')
    AND runtime.status = 'offline'
    AND COALESCE(runtime.last_seen_at, runtime.updated_at) <
        now() - make_interval(secs => @reconnect_grace_secs::double precision)
  ORDER BY COALESCE(runtime.last_seen_at, runtime.updated_at), task.created_at
  LIMIT @max_per_tick::int
  FOR UPDATE OF task SKIP LOCKED
)
UPDATE agent_task_queue AS task
SET status = 'failed', completed_at = now(), error = 'runtime went offline',
    failure_reason = 'runtime_offline',
    wait_reason = NULL
FROM victims
WHERE task.id = victims.id
  AND task.status IN ('dispatched', 'running', 'waiting_local_directory')
RETURNING task.*;

-- name: ListAgentRuntimesByOwner :many
SELECT * FROM agent_runtime
WHERE workspace_id = $1 AND owner_id = $2
ORDER BY created_at ASC;

-- name: ForceOfflineRuntimesByIDs :many
-- Unconditionally flips a known set of runtime IDs to offline. Distinct from
-- MarkRuntimesOfflineByIDs (which keeps a stale-window predicate so the
-- sweeper cannot demote a runtime that just heartbeated): this variant is
-- used by intentional revocation paths — e.g. removing a workspace member —
-- where the caller has already decided the runtime should be offline
-- regardless of recent liveness.
UPDATE agent_runtime
SET status = 'offline', updated_at = now()
WHERE id = ANY(@runtime_ids::uuid[]) AND status = 'online'
RETURNING id, workspace_id, owner_id, daemon_id, provider;

-- name: CancelAgentTasksByRuntimeOrAgent :many
-- Cancels every active task that either lives on one of the given runtimes
-- OR belongs to one of the given agents. Used by the member-revocation flow:
-- the runtime-side covers tasks queued against the leaving member's runtimes;
-- the agent-side covers tasks pinned to a different runtime that those agents
-- left behind from a prior UpdateAgent (agent.runtime_id can change, but
-- agent_task_queue.runtime_id does not get rewritten when it does, so a task
-- queued on runtime A by agent X — later moved to runtime B — survives the
-- runtime-only revoke and could still be claimed because ClaimAgentTask does
-- not gate on agent.archived_at).
--
-- We use 'cancelled' rather than 'failed' so the daemon's per-task status
-- poller (watchTaskCancellation) interrupts the running agent gracefully.
-- Returns the affected rows so the caller can broadcast task:cancelled and
-- reconcile per-agent status.
--
-- The status list must cover EVERY non-terminal status, not just the ones the
-- daemon is actively working: 'deferred' (migration 128, comment-routing
-- escalation) was missing here and only went unnoticed because the runtime
-- delete used to cascade those rows away. Since MUL-5559 the runtime delete
-- unbinds history rows instead, and agent_task_queue_active_requires_runtime
-- rejects an active row without a runtime — so a missed status now surfaces as
-- a failed delete (runtime_delete_not_drained) instead of silent data loss.
UPDATE agent_task_queue
SET status = 'cancelled', completed_at = now()
WHERE (runtime_id = ANY(@runtime_ids::uuid[]) OR agent_id = ANY(@agent_ids::uuid[]))
  AND status IN ('queued', 'dispatched', 'running', 'waiting_local_directory', 'deferred')
RETURNING *;

-- name: CountUndrainedTasksByRuntimeOrAgent :one
-- Belt-and-braces gate for the runtime-delete transaction: after cancelling,
-- every task on this runtime OR owned by an agent being unbound must be terminal
-- (completed_at IS NOT NULL) before the unbind UPDATE runs. The agent-side
-- predicate must mirror CancelAgentTasksByRuntimeOrAgent: a task can remain
-- pinned to another runtime after its agent moves. Non-zero means some
-- non-terminal status escaped the cancel query — the handler aborts with 409
-- runtime_delete_not_drained rather than letting the CHECK constraint turn it
-- into an opaque 500, and rather than deleting rows to make it go away.
SELECT count(*) FROM agent_task_queue
WHERE (runtime_id = ANY(@runtime_ids::uuid[]) OR agent_id = ANY(@agent_ids::uuid[]))
  AND completed_at IS NULL;

-- name: UnbindTasksFromRuntime :execrows
-- Detaches this runtime's task history so deleting the runtime row cannot
-- cascade it away (agent_task_queue.runtime_id is ON DELETE CASCADE, and
-- task_message / task_usage / task_token cascade from the task in turn).
-- Restricted to terminal rows: an active task must keep its runtime, per
-- agent_task_queue_active_requires_runtime. The caller runs
-- CancelAgentTasksByRuntimeOrAgent +
-- CountUndrainedTasksByRuntimeOrAgent first, so at this point "terminal" is
-- every row on the runtime.
UPDATE agent_task_queue
SET runtime_id = NULL
WHERE runtime_id = $1 AND completed_at IS NOT NULL;

-- name: UnbindUserAgentsFromRuntime :many
-- MUL-5559: the runtime-delete replacement for archive-then-hard-delete. Every
-- user agent bound to this runtime becomes unbound (runtime_id IS NULL) and
-- keeps its row, chats, labels, channel installations and autopilot config.
--
-- Deliberately NOT filtered on archived_at: an agent archived earlier is just
-- as much the user's data as an active one, and hard-deleting it was the same
-- bug. Deliberately restricted to kind = 'user': system agents are invisible
-- execution infrastructure with no UI to rebind them (see
-- DeleteSystemAgentsByRuntime), so leaving them unbound would strand rows no
-- one can repair.
UPDATE agent
SET runtime_id = NULL, updated_at = now()
WHERE runtime_id = $1 AND kind = 'user'
RETURNING *;

-- name: LockForeignAgentsByRuntime :many
-- MUL-6704: every agent bound to this runtime whose owner is NOT the runtime
-- owner — the set a public → private revoke has to deal with — locked FOR
-- UPDATE. Active and archived, both kinds: the handler splits them (user agents
-- are unbound, `kind = 'system'` carriers keep their binding, see
-- UnbindForeignUserAgentsFromRuntime) and the counts feed the confirmation
-- dialog.
--
-- The predicate is `IS DISTINCT FROM`, not `<>`, so an ownerless agent row is
-- part of the set. That matches the claim fence added in #7571, which lets a
-- NULL-owner agent through the SQL and then settles it explicitly at delivery:
-- such an agent can never actually run on a private runtime, so the plan the
-- user confirms must include it rather than leave it silently stuck.
--
-- There is deliberately NO unlocked variant. Every read of this set decides
-- whether to write — either the teardown or the bare visibility flip — and an
-- unlocked read leaves the bind race described in
-- handler.makeRuntimePrivateIfUnaffected open. Pair with LockAgentRuntime
-- (FOR UPDATE, taken first), which blocks the FK-validated INSERTs and
-- runtime_id UPDATEs that would add an agent mid-teardown; this locks the rows
-- that already exist so a concurrent archive / restore / rebind of one of them
-- blocks until we commit. Ordered by id — the same order
-- ListUserAgentsByRuntimeForUpdate uses — so the revoke and delete paths can
-- never deadlock against each other.
SELECT * FROM agent
WHERE runtime_id = $1 AND owner_id IS DISTINCT FROM @owner_id
ORDER BY id
FOR UPDATE;

-- name: UnbindForeignUserAgentsFromRuntime :many
-- The revoke counterpart of UnbindUserAgentsFromRuntime: unbinds only the user
-- agents this runtime may no longer execute, leaving the owner's own agents
-- bound. Reusing the delete-path query here would be a serious bug — it has no
-- owner filter, so reclaiming your own machine would first unbind your own
-- agents.
--
-- Archived rows included (an archived agent is just as much the user's data),
-- `kind = 'system'` excluded: a system carrier has no UI to rebind it, so
-- unbinding one strands a row nobody can repair. Those keep their binding and
-- are refused at admission instead (service.RuntimeAllowsAgentOwner).
UPDATE agent
SET runtime_id = NULL, updated_at = now()
WHERE runtime_id = $1
  AND kind = 'user'
  AND owner_id IS DISTINCT FROM @owner_id
RETURNING *;

-- name: CancelAgentTasksByAgentsWithReason :many
-- Cancels every non-terminal task of the given agents and records WHY.
--
-- Deliberately separate from CancelAgentTasksByRuntimeOrAgent rather than
-- adding a reason parameter to it: that query is shared by runtime delete and
-- member revocation, whose reasons are a different decision, and it also
-- carries a runtime-side leg this caller must NOT use (a visibility revoke
-- keeps the runtime, so matching on runtime_id would cancel the owner's own
-- tasks on their own machine).
--
-- error + failure_reason are reused instead of adding a cancel-reason column
-- (MUL-6704 decision C): failure_reason is an open string, and the clients
-- already render it for cancelled rows (cancelReasonLabel). Dashboards only
-- read it for status='failed', so a cancelled row cannot inflate error rates.
--
-- The status list mirrors CancelAgentTasksByRuntimeOrAgent — every non-terminal
-- status. wait_reason and the prepare lease are cleared: the row is terminal
-- now and a stale lease would make it look claimable to a human reading it.
UPDATE agent_task_queue
SET status = 'cancelled',
    completed_at = now(),
    error = @error,
    failure_reason = @failure_reason,
    wait_reason = NULL,
    prepare_lease_expires_at = NULL
WHERE agent_id = ANY(@agent_ids::uuid[])
  AND status IN ('queued', 'dispatched', 'running', 'waiting_local_directory', 'deferred')
RETURNING *;

-- name: CancelPreClaimTasksOnOtherRuntimes :many
-- MUL-6704: the rebind cleanup. When UpdateAgent moves an agent to another
-- runtime, agent_task_queue.runtime_id is NOT rewritten, and since #7571 the
-- claim fence requires agent.runtime_id = agent_task_queue.runtime_id. So rows
-- left pinned to the previous runtime become unclaimable from both sides — the
-- old machine is refused, the new machine never sees them — and used to sit
-- there until the 2h queue TTL failed them as the misleading `queued_expired`.
--
-- Restricted to the pre-claim statuses on purpose. A `dispatched` or `running`
-- task is already executing on the old machine and still completes correctly
-- (CompleteAgentTask does not check the binding), so cancelling it here would
-- throw away work the user never asked to stop by editing an agent. The
-- residual case — a dispatched row whose daemon never started it — is settled
-- by CancelStaleMismatchedDispatchedTasks.
UPDATE agent_task_queue
SET status = 'cancelled',
    completed_at = now(),
    error = @error,
    failure_reason = @failure_reason,
    wait_reason = NULL,
    prepare_lease_expires_at = NULL
WHERE agent_id = @agent_id
  AND runtime_id IS DISTINCT FROM @runtime_id
  AND status IN ('queued', 'waiting_local_directory', 'deferred')
RETURNING *;

-- name: CancelStaleMismatchedDispatchedTasks :many
-- Sweeper arm for MUL-6704: a `dispatched` row whose agent has since moved to
-- another runtime can no longer be reclaimed — RecoverStaleDispatchedTask and
-- its batch variant both require agent.runtime_id = agent_task_queue.runtime_id
-- since #7571 — so without this it would drift until FailStaleTasks called it a
-- `timeout`, pointing the user at a hung machine that was never involved.
--
-- Only rows past the claim-recovery window with no live prepare lease are
-- taken, which is exactly the "the daemon that claimed this is not going to
-- start it" condition the reclaim path uses. `running` is never touched here:
-- a run in flight on the old machine finishes normally.
--
-- Bounded per tick with FOR UPDATE SKIP LOCKED so a backlog cannot monopolise
-- the sweeper tick or contend with a live claim.
WITH victims AS (
    SELECT atq.id
    FROM agent_task_queue atq
    JOIN agent a ON a.id = atq.agent_id
    WHERE atq.status = 'dispatched'
      AND atq.runtime_id IS NOT NULL
      AND a.runtime_id IS DISTINCT FROM atq.runtime_id
      AND atq.dispatched_at < now() - make_interval(secs => @claim_recovery_secs::double precision)
      AND (atq.prepare_lease_expires_at IS NULL OR atq.prepare_lease_expires_at < now())
    ORDER BY atq.dispatched_at ASC
    LIMIT @max_per_tick::int
    FOR UPDATE OF atq SKIP LOCKED
)
UPDATE agent_task_queue
SET status = 'cancelled',
    completed_at = now(),
    error = @error,
    failure_reason = @failure_reason,
    wait_reason = NULL,
    prepare_lease_expires_at = NULL
WHERE id IN (SELECT id FROM victims)
  AND status = 'dispatched'
RETURNING *;

-- name: DeleteAgentRuntime :exec
DELETE FROM agent_runtime WHERE id = $1;

-- name: DeleteSystemAgentsByRuntime :exec
-- System agents are invisible execution infrastructure (for example the Agent
-- Builder). Remove them before deleting their runtime so the RESTRICT runtime
-- FK cannot block an otherwise dependency-free delete.
DELETE FROM agent WHERE runtime_id = $1 AND kind = 'system';

-- name: CountActiveAgentsByRuntime :one
SELECT count(*) FROM agent WHERE runtime_id = $1 AND archived_at IS NULL;

-- name: FindLegacyRuntimesByDaemonID :many
-- Looks up runtime rows keyed on a prior (hostname-derived) daemon_id. Used
-- at register-time to find rows owned by the same machine under its old
-- identity so agents/tasks can be re-pointed at the new UUID-keyed row.
--
-- Comparison is case-insensitive because os.Hostname() has been observed to
-- return different casings on the same machine (e.g. `Jiayuans-MacBook-Pro`
-- vs `jiayuans-macbook-pro`) across reboots/mDNS state changes. A case-
-- sensitive `=` would strand the old row; LOWER() on both sides handles drift
-- without forcing the daemon to enumerate cased permutations.
--
-- Returns many rather than one because case drift may have already minted
-- duplicate rows historically (e.g. `Foo.local` AND `foo.local` under the
-- same workspace+provider). A single-row lookup would consolidate only one
-- of them and leave the rest orphaned. Callers must merge every returned
-- row into the new UUID-keyed runtime.
SELECT * FROM agent_runtime
WHERE workspace_id = @workspace_id
  AND provider = @provider
  AND LOWER(daemon_id) = LOWER(@daemon_id);

-- name: ReassignAgentsToRuntime :execrows
-- Re-points every agent referencing old_runtime_id at new_runtime_id.
UPDATE agent
SET runtime_id = @new_runtime_id
WHERE runtime_id = @old_runtime_id;

-- name: LockWorkspaceForRuntimeMerge :exec
-- Step 1 of the legacy runtime merge's fence, and the same first step every task
-- write takes (lock_task_owner_rows, migration 284): the workspace row, FOR KEY
-- SHARE. Taking it here rather than relying on the fence inside the reassignment
-- keeps the merge's lock order identical to the writers' — workspaces before owner
-- rows — so the two can never hold each other's next lock (MUL-5999).
SELECT 1 FROM workspace w
WHERE w.id IN (
    SELECT r.workspace_id FROM agent_runtime r WHERE r.id = ANY(@runtime_ids::uuid[])
)
ORDER BY w.id
FOR KEY SHARE;

-- name: LockRuntimesForMerge :many
-- Step 2: the runtime rows themselves, FOR UPDATE, in id order.
--
-- FOR UPDATE is the point. A task write's fence takes FOR KEY SHARE on the runtime
-- it references, and KEY SHARE conflicts with UPDATE but not with another KEY
-- SHARE — so while the merge held only the workspace's KEY SHARE, a concurrent
-- enqueue against the OLD runtime went straight through after the task scan and was
-- then silently removed by ON DELETE CASCADE when the old runtime was deleted.
-- Holding FOR UPDATE on both runtimes from before the scan until COMMIT means a
-- late writer either commits first (and the scan sees its task) or waits and finds
-- the runtime gone, which its fence reports as "no row written".
--
-- Both runtimes are locked in one ordered statement so two merges running in
-- opposite directions cannot take the same pair in opposite orders.
--
-- Returns the rows it actually locked: a caller that asked for two and got fewer
-- knows a runtime disappeared before it got there and must abandon the merge.
-- Full rows rather than ids because the merge also has to read the locked
-- pre-merge state — the legacy row's `visibility`, which the surviving row
-- inherits when it was `public` (MUL-6704) — and reading that outside the lock
-- would reintroduce the race this lock exists to close.
SELECT * FROM agent_runtime
WHERE id = ANY(@runtime_ids::uuid[])
ORDER BY id
FOR UPDATE;

-- name: ReassignTasksToRuntime :one
-- Fenced against workspace teardown: lock_task_owner_rows (migration 284)
-- locks the owners' workspace rows in the writer's own transaction and returns
-- false once they are gone, so this statement writes no row instead of stranding
-- a task in a workspace that has just been deleted (MUL-5999).
-- Re-points every queued/running/completed task referencing old_runtime_id.
-- Required before deleting the old runtime row because agent_task_queue has
-- an ON DELETE CASCADE FK that would otherwise drop historical tasks.
--
-- Returns the fence verdict separately from the row count on purpose. "0 rows"
-- is ambiguous — it means either "the old runtime had no tasks" or "the fence
-- refused" — and a caller that cannot tell them apart would go on to delete the
-- old runtime, letting that same ON DELETE CASCADE drop the very history this
-- statement exists to preserve. FenceOk = false must abort the merge.
WITH fence AS MATERIALIZED (
    -- Once per statement rather than once per row: the predicate is VOLATILE, so
    -- calling it from the WHERE clause of a bulk UPDATE would re-run it for every
    -- candidate row.
    SELECT lock_task_owner_rows(NULL, NULL, @new_runtime_id) AS ok
),
reassigned AS (
    UPDATE agent_task_queue
    SET runtime_id = @new_runtime_id
    WHERE runtime_id = @old_runtime_id
      AND (SELECT ok FROM fence)
    RETURNING id
)
SELECT
    (SELECT ok FROM fence) AS fence_ok,
    (SELECT count(*) FROM reassigned) AS reassigned_tasks;

-- name: RecordRuntimeLegacyDaemonID :exec
-- Remembers the most recent hostname-derived daemon_id that was merged into
-- this row. Useful for debugging when tracing back why a given runtime row
-- subsumed an old one, and only overwrites NULL so the earliest merge is
-- preserved.
UPDATE agent_runtime
SET legacy_daemon_id = COALESCE(legacy_daemon_id, $2)
WHERE id = $1;

-- name: ListStaleOfflineRuntimeGCCandidates :many
-- Bounded gather for runtime GC. Non-terminal task owners are deliberately
-- excluded here so one permanently-deferred task cannot monopolise the front
-- of every batch and starve otherwise-drainable runtimes. The per-runtime
-- transaction re-checks every predicate after taking FOR UPDATE, so this is an
-- efficiency filter rather than the correctness boundary.
SELECT id FROM agent_runtime
WHERE status = 'offline'
  AND last_seen_at < now() - make_interval(secs => @stale_seconds::double precision)
  AND NOT EXISTS (
    SELECT 1
    FROM agent
    WHERE agent.runtime_id = agent_runtime.id
  )
  AND NOT EXISTS (
    SELECT 1
    FROM agent_task_queue
    WHERE agent_task_queue.runtime_id = agent_runtime.id
      AND agent_task_queue.completed_at IS NULL
  )
ORDER BY last_seen_at ASC, id ASC
LIMIT @max_per_tick::int;

-- name: IsAgentRuntimeEligibleForGC :one
-- Re-checks the mutable GC predicates after the caller has locked the runtime
-- row FOR UPDATE. Agent inserts/updates and task ownership writes take FOR KEY
-- SHARE on that row, so no new dependency can commit between this check and
-- DeleteAgentRuntime in the same transaction.
SELECT EXISTS (
  SELECT 1 FROM agent_runtime
  WHERE agent_runtime.id = @id
    AND status = 'offline'
    AND last_seen_at < now() - make_interval(secs => @stale_seconds::double precision)
    AND NOT EXISTS (
      SELECT 1
      FROM agent
      WHERE agent.runtime_id = agent_runtime.id
    )
) AS eligible;

-- name: CountTasksByRuntime :one
-- Final fail-closed assertion after UnbindTasksFromRuntime. A non-zero result
-- aborts the transaction instead of relying on the legacy ON DELETE CASCADE.
SELECT count(*) FROM agent_task_queue WHERE runtime_id = $1;

-- name: CountStaleOfflineRuntimesBlockedByTasks :one
-- Bounded observability sample of runtimes that are otherwise GC-eligible but
-- retain a non-terminal task. In particular, deferred tasks have no generic
-- TTL, so silently filtering them from the candidate batch would hide a
-- permanently-starved runtime. The count saturates at max_rows so this
-- recurring safety signal cannot become an unbounded backlog scan.
SELECT count(*) FROM (
  SELECT 1 FROM agent_runtime
  WHERE status = 'offline'
    AND last_seen_at < now() - make_interval(secs => @stale_seconds::double precision)
    AND NOT EXISTS (
      SELECT 1
      FROM agent
      WHERE agent.runtime_id = agent_runtime.id
    )
    AND EXISTS (
      SELECT 1
      FROM agent_task_queue
      WHERE agent_task_queue.runtime_id = agent_runtime.id
        AND agent_task_queue.completed_at IS NULL
    )
  LIMIT @max_rows::int
) AS blocked_runtimes;
