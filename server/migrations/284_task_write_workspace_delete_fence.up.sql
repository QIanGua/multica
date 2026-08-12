-- Workspace teardown needs a fence that no task write can bypass, and that does
-- not borrow its atomicity from a foreign key (MUL-5999 review).
--
-- THE WINDOW THIS CLOSES
--
-- Teardown sweeps a workspace's tasks and then commits. Anything that inserts a
-- task — or moves an existing one onto this workspace's agent, issue or runtime —
-- between the last sweep and COMMIT would leave a task behind whose workspace no
-- longer exists, together with its own leaf rows. Re-scanning before commit cannot
-- close that window: the check and the commit are not atomic with respect to a
-- concurrent inserter, so the row can always land after the final empty scan.
--
-- WHY NOT THE FOREIGN KEYS
--
-- Inserting an agent_task_queue row already takes FOR KEY SHARE on the agent,
-- issue and runtime it references, which does block against teardown's FOR UPDATE
-- on those rows. That is a side effect of FK enforcement, and the legacy FKs are
-- documented in workspace_delete.sql as an expand-phase compatibility net that a
-- later contract removes — so relying on them makes tenant-cleanup correctness a
-- hostage of a future migration.
--
-- WHY A TRIGGER RATHER THAN A CHECK IN EVERY WRITE PATH
--
-- The fence has to be in the SAME transaction as the write, otherwise the lock is
-- released before the insert lands. There are seven INSERT statements and several
-- ownership-changing UPDATEs across agent.sql, autopilot.sql, chat.sql and
-- runtime.sql, and several of them run as single autocommit statements, so a
-- separate "take the fence" statement issued by the caller would be its own
-- transaction and protect nothing. A BEFORE trigger is in-transaction by
-- construction, covers every existing path, and — unlike a convention — cannot be
-- forgotten by a path added later.
--
-- WHAT IT DOES
--
-- Takes FOR KEY SHARE on the workspace row behind every owner the new row
-- references. FOR KEY SHARE conflicts with the FOR UPDATE that
-- LockWorkspaceForDelete already holds for the whole teardown transaction, so a
-- task write against a workspace being torn down blocks until teardown finishes,
-- and then either proceeds (teardown rolled back) or fails closed with the error
-- below (teardown committed and the workspace is gone).
--
-- COST
--
-- One indexed lookup per owner column that is set, plus a shared row lock, per
-- inserted row. It fires on INSERT and only on UPDATEs that actually change an
-- ownership column, so the hot claim / dispatch / complete path — which updates
-- status, timestamps and results — does not pay for it. Teardown's own
-- parent_task_id detach does not touch an ownership column either.

CREATE OR REPLACE FUNCTION fence_task_write_against_workspace_delete()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    expected int;
    locked int;
BEGIN
    -- The distinct workspaces this row belongs to. A task's agent, runtime and
    -- issue are not guaranteed to share a workspace, so all of them count.
    WITH owners AS (
        SELECT workspace_id FROM agent WHERE id = NEW.agent_id
        UNION
        SELECT workspace_id FROM issue WHERE id = NEW.issue_id
        UNION
        SELECT workspace_id FROM agent_runtime WHERE id = NEW.runtime_id
    )
    SELECT count(*) INTO expected FROM owners;

    -- No resolvable owner at all means the referencing rows are gone; let the
    -- foreign keys report that in their own words.
    IF expected = 0 THEN
        RETURN NEW;
    END IF;

    -- The lock and the count have to be at different query levels: PostgreSQL
    -- rejects a locking clause in the same SELECT as an aggregate.
    WITH locked_workspaces AS (
        SELECT w.id
        FROM workspace w
        WHERE w.id IN (
            SELECT workspace_id FROM agent WHERE id = NEW.agent_id
            UNION
            SELECT workspace_id FROM issue WHERE id = NEW.issue_id
            UNION
            SELECT workspace_id FROM agent_runtime WHERE id = NEW.runtime_id
        )
        FOR KEY SHARE
    )
    SELECT count(*) INTO locked FROM locked_workspaces;

    IF locked < expected THEN
        RAISE EXCEPTION
            'cannot write agent_task_queue row: its workspace is being deleted or no longer exists'
            USING ERRCODE = 'foreign_key_violation';
    END IF;

    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_atq_fence_workspace_delete ON agent_task_queue;
CREATE TRIGGER trg_atq_fence_workspace_delete
BEFORE INSERT OR UPDATE OF agent_id, issue_id, runtime_id ON agent_task_queue
FOR EACH ROW
EXECUTE FUNCTION fence_task_write_against_workspace_delete();
