-- Single statement, CONCURRENTLY: dropping an index on a hot table must not take
-- ACCESS EXCLUSIVE for the duration.
--
-- idx_agent_task_queue_issue_id (migration 035) is fully superseded by
-- idx_agent_task_queue_issue_id_keyset (migration 279): same leading column, so
-- the planner can use it for every predicate 035 served. Keeping both would make
-- every insert and every issue_id update maintain two btrees on the largest
-- table in the database.
DROP INDEX CONCURRENTLY IF EXISTS idx_agent_task_queue_issue_id;
