-- Single statement: CREATE INDEX CONCURRENTLY cannot run inside a transaction
-- or share a multi-command migration file.
--
-- Same keyset-paging reason as migration 278, for teardown's issue path.
--
-- This supersedes idx_agent_task_queue_issue_id (migration 035): a btree on
-- (issue_id, id) serves every `issue_id = $1` predicate that the single-column
-- index served, and additionally produces id order. Migration 280 drops the
-- superseded index once this one exists, so the hot table does not carry both.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_agent_task_queue_issue_id_keyset
    ON agent_task_queue (issue_id, id);
