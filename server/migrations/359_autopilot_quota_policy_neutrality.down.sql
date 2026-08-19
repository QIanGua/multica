ALTER TABLE autopilot_quota_reservation
    ADD CONSTRAINT autopilot_quota_reservation_source_check
    CHECK (source IN ('schedule', 'manual', 'webhook', 'api')) NOT VALID;

ALTER TABLE autopilot_quota_period
    DROP COLUMN IF EXISTS id;
