-- The write half of workspace teardown's concurrency contract (MUL-5999).
--
-- THE WINDOW
--
-- Teardown sweeps a workspace's tasks and then commits. Anything that inserts a
-- task — or moves an existing one onto this workspace's agent, issue or runtime —
-- between the last sweep and COMMIT would leave a task behind whose owners no
-- longer exist, together with its own leaf rows. Re-scanning before commit cannot
-- close that: the check and the commit are not atomic with respect to a
-- concurrent writer, so a row can always land after the final empty scan.
--
-- WHAT THIS IS
--
-- A predicate that every statement writing agent_task_queue ownership calls in
-- its own WHERE clause:
--
--     INSERT INTO agent_task_queue (...)
--     SELECT ... WHERE lock_task_owner_workspaces(@agent_id, @issue_id, @runtime_id)
--
-- It resolves the workspaces behind the row's agent, issue and runtime, locks
-- those workspace rows FOR KEY SHARE, and returns false unless every owner
-- reference resolved and every one of their workspaces is still there. FOR KEY
-- SHARE conflicts with the FOR UPDATE that LockWorkspaceForDelete holds for the
-- whole teardown transaction, so a task write against a workspace being torn down
-- blocks until teardown finishes and then either proceeds (teardown rolled back)
-- or writes no row (teardown committed).
--
-- WHY A CALLED PREDICATE RATHER THAN A TRIGGER OR THE FOREIGN KEYS
--
--   * Not the foreign keys. An INSERT does take FOR KEY SHARE on the rows it
--     references, which is why this window has been closed so far — but that is a
--     side effect of FK enforcement, and workspace_delete.sql documents the legacy
--     FKs as an expand-phase net a later contract removes. Cleanup correctness
--     must not be a hostage of that migration.
--   * Not a trigger. A row trigger is a database-layer relational constraint that
--     coordinates application behaviour behind the caller's back. This repo keeps
--     relationships in the application layer, so the write path names its own
--     fence, and `TestTaskOwnershipWritesCallTheFence` fails the build if a
--     statement that writes ownership forgets to.
--   * It must be in the writer's transaction. Several enqueue statements run as
--     single autocommit statements, so a separate "take the fence" statement
--     issued by the caller would be its own transaction and release the lock
--     before the insert landed. Calling the predicate from the writing statement
--     is in-transaction by construction.
--
-- DETERMINISTIC LOCK ORDER
--
-- A task's agent, runtime and issue are not guaranteed to share a workspace, so
-- this can lock up to three workspace rows. They are locked in id order: LockRows
-- sits above Sort in the plan, so `ORDER BY w.id ... FOR KEY SHARE` acquires the
-- locks in that order. Two concurrent cross-workspace writes touching the same
-- pair therefore cannot take them in opposite orders and deadlock.
--
-- VOLATILE (the default) is load-bearing: it stops the planner from inlining the
-- body or optimising the call away, which would drop the lock.

CREATE OR REPLACE FUNCTION lock_task_owner_workspaces(
    p_agent_id uuid,
    p_issue_id uuid,
    p_runtime_id uuid
)
RETURNS boolean
LANGUAGE plpgsql
AS $$
DECLARE
    required int := (CASE WHEN p_agent_id IS NULL THEN 0 ELSE 1 END)
                  + (CASE WHEN p_issue_id IS NULL THEN 0 ELSE 1 END)
                  + (CASE WHEN p_runtime_id IS NULL THEN 0 ELSE 1 END);
    resolved int;
    distinct_workspaces int;
    locked int;
BEGIN
    -- A row with no owner reference at all cannot belong to any workspace.
    IF required = 0 THEN
        RETURN TRUE;
    END IF;

    WITH owners AS (
        SELECT a.workspace_id FROM agent a WHERE a.id = p_agent_id
        UNION ALL
        SELECT i.workspace_id FROM issue i WHERE i.id = p_issue_id
        UNION ALL
        SELECT r.workspace_id FROM agent_runtime r WHERE r.id = p_runtime_id
    )
    SELECT count(*), count(DISTINCT workspace_id)
    INTO resolved, distinct_workspaces
    FROM owners;

    -- An owner reference that no longer resolves means its row has been deleted —
    -- by a teardown that already committed, most likely. Refuse without leaning
    -- on the foreign key to report it.
    IF resolved <> required THEN
        RETURN FALSE;
    END IF;

    WITH locked_workspaces AS (
        SELECT w.id
        FROM workspace w
        WHERE w.id IN (
            SELECT a.workspace_id FROM agent a WHERE a.id = p_agent_id
            UNION
            SELECT i.workspace_id FROM issue i WHERE i.id = p_issue_id
            UNION
            SELECT r.workspace_id FROM agent_runtime r WHERE r.id = p_runtime_id
        )
        ORDER BY w.id
        FOR KEY SHARE
    )
    SELECT count(*) INTO locked FROM locked_workspaces;

    RETURN locked = distinct_workspaces;
END;
$$;
