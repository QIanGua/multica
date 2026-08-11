CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_agent_task_queue_issue_id
    ON agent_task_queue (issue_id);
