-- Keep the table-creation migration immutable: add the period identifier and
-- loosen execution-source storage in an explicit follow-up migration.

ALTER TABLE autopilot_quota_period
    ADD COLUMN id UUID NOT NULL DEFAULT gen_random_uuid();

ALTER TABLE autopilot_quota_reservation
    DROP CONSTRAINT autopilot_quota_reservation_source_check;
