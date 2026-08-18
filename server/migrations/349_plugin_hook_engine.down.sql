ALTER TABLE comment DROP CONSTRAINT IF EXISTS comment_author_type_check;
ALTER TABLE comment ADD CONSTRAINT comment_author_type_check
    CHECK (author_type IN ('member', 'agent', 'system'));
ALTER TABLE plugin_installation DROP COLUMN IF EXISTS token_rotated_at;
ALTER TABLE plugin_installation DROP COLUMN IF EXISTS token_hash;
DROP TABLE IF EXISTS plugin_invocation;
