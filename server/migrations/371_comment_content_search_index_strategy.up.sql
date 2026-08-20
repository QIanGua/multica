-- The migration runner executes this only when the CJK-friendly
-- idx_comment_content_bigm index has the exact valid and ready shape used by
-- SearchIssues. pg_bigm-less self-hosted deployments skip this statement and
-- retain the trigram fallback.
DROP INDEX CONCURRENTLY IF EXISTS idx_comment_content_trgm;
