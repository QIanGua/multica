package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/testutil/plugintest"
)

func pluginHandlerRequest(method, path string, body []byte, params map[string]string) *http.Request {
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.Header.Set("X-User-ID", testUserID)
	request.Header.Set("Content-Type", "application/json")
	routeContext := chi.NewRouteContext()
	for key, value := range params {
		routeContext.URLParams.Add(key, value)
	}
	return request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
}

func TestPluginHTTPLifecycleForInstalledReferenceRelease(t *testing.T) {
	cleanup := func() {
		ctx := context.Background()
		testPool.Exec(ctx, `DELETE FROM plugin_health WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM plugin_capability_snapshot WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM plugin_workspace_capability_state WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM plugin_binding WHERE installation_id IN (SELECT id FROM plugin_installation WHERE workspace_id = $1)`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM plugin_grant WHERE installation_id IN (SELECT id FROM plugin_installation WHERE workspace_id = $1)`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM plugin_installation WHERE workspace_id = $1`, testWorkspaceID)
	}
	cleanup()
	t.Cleanup(cleanup)

	recorder := httptest.NewRecorder()
	testHandler.ListPluginCatalog(recorder, pluginHandlerRequest(http.MethodGet, "/plugins/catalog", nil, map[string]string{"id": testWorkspaceID}))
	if recorder.Code != http.StatusOK || !bytes.Contains(recorder.Body.Bytes(), []byte(`"signature_verified":true`)) || !bytes.Contains(recorder.Body.Bytes(), []byte(plugintest.ReviewReadinessPluginKey)) {
		t.Fatalf("catalog status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var catalogPayload struct {
		Diagnostics json.RawMessage `json:"diagnostics"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &catalogPayload); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	if !bytes.Equal(catalogPayload.Diagnostics, []byte(`[]`)) {
		t.Fatalf("empty catalog diagnostics must be an array, got %s", catalogPayload.Diagnostics)
	}

	recorder = httptest.NewRecorder()
	testHandler.InstallPlugin(recorder, pluginHandlerRequest(
		http.MethodPost,
		"/plugins/install",
		[]byte(`{"plugin_key":"ai.multica.software-delivery","version":"1.0.0"}`),
		map[string]string{"id": testWorkspaceID},
	))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("install status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var installed pluginInstallationResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &installed); err != nil {
		t.Fatalf("decode installation: %v", err)
	}
	if installed.Enabled || installed.LifecycleStatus != "installed" {
		t.Fatalf("install must remain disabled: %+v", installed)
	}

	params := map[string]string{"id": testWorkspaceID, "installationId": installed.ID}
	recorder = httptest.NewRecorder()
	testHandler.EnablePlugin(recorder, pluginHandlerRequest(
		http.MethodPost,
		"/plugins/enable",
		[]byte(`{"scope_type":"workspace","scope_id":"00000000-0000-0000-0000-000000000001"}`),
		params,
	))
	if recorder.Code != http.StatusNotFound || bytes.Contains(recorder.Body.Bytes(), []byte("no rows")) {
		t.Fatalf("cross-workspace binding status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	testHandler.EnablePlugin(recorder, pluginHandlerRequest(http.MethodPost, "/plugins/enable", nil, params))
	if recorder.Code != http.StatusOK {
		t.Fatalf("enable status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	testHandler.ListPlugins(recorder, pluginHandlerRequest(http.MethodGet, "/plugins", nil, map[string]string{"id": testWorkspaceID}))
	if recorder.Code != http.StatusOK || !bytes.Contains(recorder.Body.Bytes(), []byte(plugintest.ReviewReadinessPluginKey)) {
		t.Fatalf("list status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	testHandler.UpgradePlugin(recorder, pluginHandlerRequest(
		http.MethodPost,
		"/plugins/upgrade",
		[]byte(`{"plugin_key":"ai.multica.software-delivery","version":"1.1.0"}`),
		params,
	))
	if recorder.Code != http.StatusOK || !bytes.Contains(recorder.Body.Bytes(), []byte(`"desired_version":"1.1.0"`)) {
		t.Fatalf("upgrade status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	testHandler.UpgradePlugin(recorder, pluginHandlerRequest(
		http.MethodPost,
		"/plugins/upgrade",
		[]byte(`{"plugin_key":"ai.multica.software-delivery","version":"1.0.0"}`),
		params,
	))
	if recorder.Code != http.StatusConflict || !bytes.Contains(recorder.Body.Bytes(), []byte("not newer")) {
		t.Fatalf("downgrade through upgrade status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	testHandler.DisablePlugin(recorder, pluginHandlerRequest(http.MethodPost, "/plugins/disable", nil, params))
	if recorder.Code != http.StatusOK {
		t.Fatalf("disable status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	testHandler.RollbackPlugin(recorder, pluginHandlerRequest(http.MethodPost, "/plugins/rollback", []byte(`{"version":"1.0.0"}`), params))
	if recorder.Code != http.StatusOK || !bytes.Contains(recorder.Body.Bytes(), []byte(`"desired_version":"1.0.0"`)) {
		t.Fatalf("rollback status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	testHandler.RollbackPlugin(recorder, pluginHandlerRequest(http.MethodPost, "/plugins/rollback", []byte(`{"version":"9.9.9"}`), params))
	if recorder.Code != http.StatusNotFound || bytes.Contains(recorder.Body.Bytes(), []byte("no rows")) {
		t.Fatalf("safe missing rollback status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
