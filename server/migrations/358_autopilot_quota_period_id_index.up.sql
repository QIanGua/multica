-- Backing index for autopilot_quota_period's primary key. It is built in its
-- own migration so CONCURRENTLY runs outside an implicit transaction.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS autopilot_quota_period_pkey_uidx
    ON autopilot_quota_period(id);
