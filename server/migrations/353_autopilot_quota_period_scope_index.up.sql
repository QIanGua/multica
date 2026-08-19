CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_autopilot_quota_period_scope
    ON autopilot_quota_period(workspace_id, period_start, period_end);
