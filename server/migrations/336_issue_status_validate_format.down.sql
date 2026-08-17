-- Re-marking a validated constraint as NOT VALID is not expressible in
-- Postgres, and dropping it here would fight migration 335's down direction.
-- Rolling back validation alone is a no-op.
SELECT 1;
