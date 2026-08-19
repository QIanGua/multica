CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_autopilot_quota_reservation_id
    ON autopilot_quota_reservation(id);
