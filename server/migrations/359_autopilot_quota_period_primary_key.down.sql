-- Dropping the constraint also drops the attached backing index, so migration
-- 358's down direction becomes a safe no-op.
ALTER TABLE autopilot_quota_period
    DROP CONSTRAINT IF EXISTS autopilot_quota_period_pkey;
