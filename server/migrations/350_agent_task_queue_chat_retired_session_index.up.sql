CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_agent_task_queue_chat_retired_session
ON agent_task_queue (chat_session_id, retired_session_id)
WHERE chat_session_id IS NOT NULL
  AND retired_session_id IS NOT NULL;
