CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_agent_task_queue_chat_terminal_resume
ON agent_task_queue (chat_session_id, session_id, completed_at DESC)
INCLUDE (status, failure_reason, runtime_id, work_dir)
WHERE chat_session_id IS NOT NULL
  AND status IN ('completed', 'failed', 'cancelled');
