-- Dropping the constraint also drops the attached backing index, so migration
-- 354's down direction becomes a safe no-op.
ALTER TABLE autopilot_quota_reservation
    DROP CONSTRAINT IF EXISTS autopilot_quota_reservation_pkey;
