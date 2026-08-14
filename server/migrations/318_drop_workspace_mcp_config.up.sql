-- Drops the inherit-by-default column added in 314. Its model — every agent
-- picks the workspace document up automatically — is replaced by the explicit
-- assignment in 315, so leaving the column would keep a second, silently
-- inherited source of MCP servers alive.
--
-- No data migration: the column shipped in the same unreleased cycle as its
-- replacement, and auto-converting each workspace document into library
-- entries would ASSIGN them to every agent, which is exactly the behaviour
-- being removed. Admins re-add what they want shared and assign it.
ALTER TABLE workspace DROP COLUMN IF EXISTS mcp_config;
