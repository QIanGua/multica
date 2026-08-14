package handler

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/multica-ai/multica/server/internal/logger"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// WorkspaceMcpConfigResponse is the read/write shape of the shared MCP
// document. It mirrors the agent resource's contract: the secret-bearing
// document itself is dropped (and McpConfigRedacted set) for callers who may
// not read it, while coarse, non-secret metadata — how many servers are shared
// — stays visible so a member can tell the workspace has one configured.
type WorkspaceMcpConfigResponse struct {
	WorkspaceID       string          `json:"workspace_id"`
	McpConfig         json.RawMessage `json:"mcp_config"`
	McpConfigRedacted bool            `json:"mcp_config_redacted"`
	ServerCount       int             `json:"server_count"`
}

// buildWorkspaceMcpConfigResponse assembles the response for a workspace row,
// applying the same visibility rules mcp_config follows on agents:
//
//   - agent actors NEVER see it, even when their host's PAT would satisfy the
//     role gate (the lateral-movement vector MUL-2600 closed for custom_env);
//   - the workspace-level always-redact setting wins over any role;
//   - otherwise only owner/admin — the roles that may write it — read it back.
//
// ServerCount is computed from the stored document before redaction: a count
// carries no credential material and is what lets a non-admin's UI say
// "3 servers shared by this workspace" without showing the servers.
func (h *Handler) buildWorkspaceMcpConfigResponse(ws db.Workspace, actorType, memberRole string) WorkspaceMcpConfigResponse {
	resp := WorkspaceMcpConfigResponse{WorkspaceID: uuidToString(ws.ID)}
	raw := json.RawMessage(ws.McpConfig)
	if !hasManagedJSON(raw) {
		return resp
	}
	if _, names, err := parseMcpDocument(raw); err == nil {
		resp.ServerCount = len(names)
	}
	if actorType == "agent" || workspaceAlwaysRedactSecrets(ws.Settings) || !roleAllowed(memberRole, "owner", "admin") {
		resp.McpConfigRedacted = true
		return resp
	}
	resp.McpConfig = raw
	return resp
}

// GetWorkspaceMcpConfig returns the workspace's shared MCP document.
// Member-visible so the agent settings UI can show what an agent inherits;
// the document itself is redacted for everyone who cannot write it.
func (h *Handler) GetWorkspaceMcpConfig(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "id")
	idUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	member, ok := h.workspaceMember(w, r, workspaceID)
	if !ok {
		return
	}
	ws, err := h.Queries.GetWorkspace(r.Context(), idUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "workspace not found")
		return
	}
	actorType, _ := h.resolveActor(r, requestUserID(r), workspaceID)
	writeJSON(w, http.StatusOK, h.buildWorkspaceMcpConfigResponse(ws, actorType, member.Role))
}

// UpdateWorkspaceMcpConfigRequest carries the whole shared document. The field
// is a raw message so an absent key (nothing to do) stays distinguishable from
// an explicit `null` (clear the workspace layer) — the same three-state
// contract the agent column uses.
type UpdateWorkspaceMcpConfigRequest struct {
	McpConfig json.RawMessage `json:"mcp_config"`
}

// UpdateWorkspaceMcpConfig replaces the workspace's shared MCP document.
// Admin-gated by the router; the write is a full replace rather than a merge
// so removing a shared server is expressible.
func (h *Handler) UpdateWorkspaceMcpConfig(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "id")
	idUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	member, ok := h.workspaceMember(w, r, workspaceID)
	if !ok {
		return
	}
	// Defense in depth: the router already gates this route on owner/admin,
	// but an agent actor running under an owner's PAT would satisfy that gate.
	// Writing shared MCP servers must stay a human admin action.
	actorType, _ := h.resolveActor(r, requestUserID(r), workspaceID)
	if actorType == "agent" {
		writeError(w, http.StatusForbidden, "agents cannot modify the workspace MCP configuration")
		return
	}
	if !roleAllowed(member.Role, "owner", "admin") {
		writeError(w, http.StatusForbidden, "insufficient permissions")
		return
	}

	var req UpdateWorkspaceMcpConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(bytes.TrimSpace(req.McpConfig)) == 0 {
		writeError(w, http.StatusBadRequest, "mcp_config is required; pass null to clear the workspace configuration")
		return
	}

	var (
		ws  db.Workspace
		err error
	)
	if bytes.Equal(bytes.TrimSpace(req.McpConfig), []byte("null")) {
		ws, err = h.Queries.ClearWorkspaceMcpConfig(r.Context(), idUUID)
	} else {
		if verr := validateWorkspaceMcpConfig(req.McpConfig); verr != nil {
			writeError(w, http.StatusBadRequest, verr.Error())
			return
		}
		ws, err = h.Queries.SetWorkspaceMcpConfig(r.Context(), db.SetWorkspaceMcpConfigParams{
			ID:        idUUID,
			McpConfig: append([]byte(nil), req.McpConfig...),
		})
	}
	if err != nil {
		slog.Warn("update workspace mcp_config failed", append(logger.RequestAttrs(r), "error", err, "workspace_id", workspaceID)...)
		writeError(w, http.StatusInternalServerError, "failed to update workspace mcp_config")
		return
	}

	resp := h.buildWorkspaceMcpConfigResponse(ws, actorType, member.Role)
	// Server count only — never the document — so the audit trail cannot
	// become a second copy of the workspace's credentials.
	slog.Info("workspace mcp_config updated", append(logger.RequestAttrs(r),
		"workspace_id", workspaceID, "server_count", resp.ServerCount)...)
	writeJSON(w, http.StatusOK, resp)
}
