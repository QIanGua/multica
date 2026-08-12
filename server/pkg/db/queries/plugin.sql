-- name: CreatePluginIdentity :one
INSERT INTO plugin_identity (
    plugin_key, display_name, publisher_id, publisher_type, trust_tier
) VALUES (
    @plugin_key, @display_name, @publisher_id, @publisher_type, @trust_tier
)
RETURNING *;

-- name: GetPluginIdentity :one
SELECT * FROM plugin_identity
WHERE id = $1;

-- name: GetPluginIdentityByKey :one
SELECT * FROM plugin_identity
WHERE plugin_key = $1;

-- name: RetirePluginIdentity :one
UPDATE plugin_identity
SET retired_at = COALESCE(retired_at, now())
WHERE id = $1
RETURNING *;

-- name: CreatePluginRelease :one
WITH parent AS MATERIALIZED (
    SELECT plugin_identity.id
    FROM plugin_identity
    WHERE plugin_identity.id = @plugin_id AND plugin_identity.retired_at IS NULL
    FOR KEY SHARE
)
INSERT INTO plugin_release (
    plugin_id, version, manifest, manifest_digest,
    source_kind, source_ref,
    archive_digest, artifact_ref, artifact_digest, artifact_size,
    signature, signature_key_id
)
SELECT
    parent.id, @version, @manifest, @manifest_digest,
    @source_kind, @source_ref,
    @archive_digest, @artifact_ref, @artifact_digest, @artifact_size,
    sqlc.narg('signature'), sqlc.narg('signature_key_id')
FROM parent
RETURNING *;

-- name: GetPluginRelease :one
SELECT * FROM plugin_release
WHERE id = $1;

-- name: GetPluginReleaseByVersion :one
SELECT * FROM plugin_release
WHERE plugin_id = $1 AND version = $2;

-- name: RevokePluginRelease :one
UPDATE plugin_release
SET revocation_status = @revocation_status,
    revoked_at = now(),
    revocation_reason = @revocation_reason
WHERE id = @id AND revocation_status = 'active'
RETURNING *;

-- name: CreatePluginContribution :one
WITH parent AS MATERIALIZED (
    SELECT plugin_release.id, plugin_release.artifact_digest
    FROM plugin_release
    WHERE plugin_release.id = @release_id AND plugin_release.revocation_status = 'active'
    FOR KEY SHARE
)
INSERT INTO plugin_contribution (
    release_id, contribution_key, type, schema_version,
    display_name, description, entry_path, entry_digest,
    artifact_digest, required_daemon_features, ordinal
)
SELECT
    parent.id, @contribution_key, @type, @schema_version,
    @display_name, @description, @entry_path, @entry_digest,
    parent.artifact_digest, @required_daemon_features, @ordinal
FROM parent
RETURNING *;

-- name: ListPluginContributionsByRelease :many
SELECT * FROM plugin_contribution
WHERE release_id = $1
ORDER BY ordinal, id;

-- name: CreatePluginInstallation :one
WITH parents AS MATERIALIZED (
    SELECT w.id AS workspace_id, p.id AS plugin_id, r.id AS release_id,
           r.source_kind, r.source_ref
    FROM workspace w
    JOIN plugin_identity p ON p.id = @plugin_id AND p.retired_at IS NULL
    JOIN plugin_release r ON r.id = @release_id
                         AND r.plugin_id = p.id
                         AND r.revocation_status = 'active'
    WHERE w.id = @workspace_id
    FOR KEY SHARE OF w, p, r
)
INSERT INTO plugin_installation (
    workspace_id, plugin_id, source_kind, source_ref,
    desired_release_id, enabled, lifecycle_status,
    installed_by, updated_by
)
SELECT
    parents.workspace_id, parents.plugin_id, parents.source_kind, parents.source_ref,
    parents.release_id, FALSE, 'installed',
    sqlc.narg('installed_by'), sqlc.narg('installed_by')
FROM parents
RETURNING *;

-- name: GetPluginInstallation :one
SELECT * FROM plugin_installation
WHERE id = $1;

-- name: GetWorkspacePluginInstallation :one
SELECT * FROM plugin_installation
WHERE workspace_id = $1 AND plugin_id = $2 AND uninstalled_at IS NULL;

-- name: ListWorkspacePluginInstallations :many
SELECT * FROM plugin_installation
WHERE workspace_id = $1 AND uninstalled_at IS NULL
ORDER BY installed_at, id;

-- name: SetPluginInstallationDesiredState :one
WITH workspace_guard AS MATERIALIZED (
    SELECT workspace.id
    FROM workspace
    WHERE workspace.id = @workspace_id
    FOR KEY SHARE
),
target AS MATERIALIZED (
    SELECT i.id, r.id AS release_id
    FROM plugin_installation i
    JOIN workspace_guard w ON w.id = i.workspace_id
    JOIN plugin_release r ON r.id = @desired_release_id
                         AND r.plugin_id = i.plugin_id
                         AND r.revocation_status = 'active'
    WHERE i.id = @id
      AND i.workspace_id = @workspace_id
      AND i.uninstalled_at IS NULL
    FOR UPDATE OF i
)
UPDATE plugin_installation i
SET desired_release_id = target.release_id,
    enabled = @enabled,
    desired_generation = i.desired_generation + 1,
    lifecycle_status = 'activating',
    updated_by = sqlc.narg('updated_by'),
    updated_at = now(),
    disabled_at = CASE WHEN @enabled::boolean THEN NULL ELSE now() END
FROM target
WHERE i.id = target.id
RETURNING i.*;

-- name: CreatePluginGrantRevision :one
WITH target AS MATERIALIZED (
    SELECT i.id
    FROM plugin_installation i
    JOIN workspace w ON w.id = i.workspace_id
    WHERE i.id = @installation_id
      AND i.workspace_id = @workspace_id
      AND i.uninstalled_at IS NULL
    FOR UPDATE OF i
    FOR KEY SHARE OF w
),
next_revision AS (
    SELECT COALESCE(MAX(g.grant_revision), 0) + 1 AS revision
    FROM plugin_grant g
    JOIN target ON target.id = g.installation_id
    WHERE g.capability = @capability
)
INSERT INTO plugin_grant (
    installation_id, capability, decision, limits,
    grant_revision, approved_by, revoked_at
)
SELECT
    target.id, @capability, @decision, @limits,
    next_revision.revision, sqlc.narg('approved_by'),
    CASE WHEN @decision::text = 'denied' THEN now() ELSE NULL END
FROM target CROSS JOIN next_revision
RETURNING *;

-- name: ListLatestPluginGrants :many
SELECT DISTINCT ON (capability) *
FROM plugin_grant
WHERE installation_id = $1
ORDER BY capability, grant_revision DESC;

-- name: CreatePluginBindingRevision :one
WITH workspace_scope AS MATERIALIZED (
    SELECT i.id
    FROM plugin_installation i
    JOIN workspace w ON w.id = i.workspace_id
    WHERE @scope_type::text = 'workspace'
      AND i.id = @installation_id
      AND i.workspace_id = @workspace_id
      AND @scope_id::uuid = i.workspace_id
      AND i.uninstalled_at IS NULL
    FOR UPDATE OF i
    FOR KEY SHARE OF w
),
agent_scope AS MATERIALIZED (
    SELECT i.id
    FROM plugin_installation i
    JOIN workspace w ON w.id = i.workspace_id
    JOIN agent a ON a.id = @scope_id AND a.workspace_id = i.workspace_id
    WHERE @scope_type::text = 'agent'
      AND i.id = @installation_id
      AND i.workspace_id = @workspace_id
      AND i.uninstalled_at IS NULL
    FOR UPDATE OF i
    FOR KEY SHARE OF w, a
),
target AS MATERIALIZED (
    SELECT id FROM workspace_scope
    UNION ALL
    SELECT id FROM agent_scope
),
next_revision AS (
    SELECT COALESCE(MAX(b.binding_revision), 0) + 1 AS revision
    FROM plugin_binding b
    JOIN target ON target.id = b.installation_id
    WHERE b.scope_type = @scope_type AND b.scope_id = @scope_id
)
INSERT INTO plugin_binding (
    installation_id, scope_type, scope_id, enabled,
    binding_revision, created_by
)
SELECT
    target.id, @scope_type, @scope_id, @enabled,
    next_revision.revision, sqlc.narg('created_by')
FROM target CROSS JOIN next_revision
RETURNING *;

-- name: ListLatestPluginBindings :many
SELECT DISTINCT ON (scope_type, scope_id) *
FROM plugin_binding
WHERE installation_id = $1
ORDER BY scope_type, scope_id, binding_revision DESC;
