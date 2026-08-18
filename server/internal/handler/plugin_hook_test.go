package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/pkg/plugincontract"
)

// hookHandlerTestManifest declares the three host-driven triggers. Its endpoint
// is never reached in these tests — they cover the checks BEFORE the request
// leaves, which is where a wrong answer is a security bug rather than a broken
// integration. The wire format and signature are covered end-to-end against a
// live server in internal/service.
const hookHandlerTestManifest = `{
  "manifest_version": 1,
  "key": "com.example.hooked",
  "name": "Hooked",
  "version": "1.0.0",
  "author": { "name": "example" },
  "scopes": ["issues:read", "comments:write", "net:example.com"],
  "contributes": {
    "hooks": [{
      "key": "summarize",
      "name": "Summarize",
      "description": "Compress the discussion.",
      "triggers": ["ui", "manual", "event"],
      "events": ["issue.created"],
      "transport": { "type": "http", "url": "https://example.com/hooks/summarize" }
    }, {
      "key": "manual_only",
      "name": "Manual only",
      "description": "Only ever picked from a menu.",
      "triggers": ["manual"],
      "transport": { "type": "http", "url": "https://example.com/hooks/manual" }
    }]
  }
}`

func installHookPlugin(t *testing.T) string {
	t.Helper()
	source := withLocalPluginSource(t, hookHandlerTestManifest)
	body, _ := json.Marshal(map[string]any{
		"source_url":     source,
		"granted_scopes": []string{"issues:read", "comments:write", "net:example.com"},
	})
	recorder := httptest.NewRecorder()
	testHandler.InstallPlugin(recorder, pluginHandlerRequest(http.MethodPost, "/plugins", body, map[string]string{"id": testWorkspaceID}))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("install hook plugin: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var installed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &installed); err != nil {
		t.Fatalf("decode installation: %v", err)
	}
	return installed.ID
}

func invokeHookRequest(installationID, hookKey string, payload map[string]any) *http.Request {
	body, _ := json.Marshal(payload)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/plugin/hooks/"+hookKey, bytes.NewReader(body))
	request.Header.Set("X-User-ID", testUserID)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(pluginInstallationHeader, installationID)
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("key", hookKey)
	return request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
}

// `event` is dispatched by the host, off the event bus, precisely so that its
// writes carry the plugin's identity rather than a person's. A browser asking
// for one would be electing to run as somebody it is not.
func TestInvokePluginHookRefusesHostDrivenTriggers(t *testing.T) {
	withPluginsV1Flag(t, testHandler, true)
	cleanupPluginInstallations(t)
	installationID := installHookPlugin(t)

	for _, trigger := range []string{plugincontract.TriggerEvent, plugincontract.TriggerAgent, "", "nonsense"} {
		recorder := httptest.NewRecorder()
		testHandler.InvokePluginHook(recorder, invokeHookRequest(installationID, "summarize", map[string]any{"trigger": trigger}))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("trigger %q: status=%d body=%s, want 400", trigger, recorder.Code, recorder.Body.String())
		}
	}
}

// A hook may only be reached through a trigger its own manifest declared, even
// when the host supports that trigger in general.
func TestInvokePluginHookRefusesTriggerTheManifestDidNotDeclare(t *testing.T) {
	withPluginsV1Flag(t, testHandler, true)
	cleanupPluginInstallations(t)
	installationID := installHookPlugin(t)

	recorder := httptest.NewRecorder()
	// manual_only declares manual, not ui.
	testHandler.InvokePluginHook(recorder, invokeHookRequest(installationID, "manual_only", map[string]any{"trigger": "ui"}))
	if recorder.Code == http.StatusOK {
		t.Fatalf("a hook was invoked through a trigger it never declared: %s", recorder.Body.String())
	}
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s, want 403", recorder.Code, recorder.Body.String())
	}
}

func TestInvokePluginHookRefusesUnknownHook(t *testing.T) {
	withPluginsV1Flag(t, testHandler, true)
	cleanupPluginInstallations(t)
	installationID := installHookPlugin(t)

	recorder := httptest.NewRecorder()
	testHandler.InvokePluginHook(recorder, invokeHookRequest(installationID, "not_declared", map[string]any{"trigger": "manual"}))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want 404", recorder.Code, recorder.Body.String())
	}
}

// The flag gates the hook endpoint like every other plugin route: fail closed,
// not merely hidden from the UI.
func TestInvokePluginHookRequiresTheFeatureFlag(t *testing.T) {
	withPluginsV1Flag(t, testHandler, true)
	cleanupPluginInstallations(t)
	installationID := installHookPlugin(t)

	withPluginsV1Flag(t, testHandler, false)
	recorder := httptest.NewRecorder()
	testHandler.InvokePluginHook(recorder, invokeHookRequest(installationID, "summarize", map[string]any{"trigger": "manual"}))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s, want 503", recorder.Code, recorder.Body.String())
	}
}

// A disabled installation is off, not hidden. A stale tab must not keep
// invoking hooks after an admin switches the plugin off.
func TestInvokePluginHookRefusesDisabledInstallation(t *testing.T) {
	withPluginsV1Flag(t, testHandler, true)
	cleanupPluginInstallations(t)
	installationID := installHookPlugin(t)

	disable := httptest.NewRecorder()
	testHandler.DisablePlugin(disable, pluginHandlerRequest(http.MethodPost, "/disable", nil, map[string]string{
		"id": testWorkspaceID, "installationId": installationID,
	}))
	if disable.Code != http.StatusOK {
		t.Fatalf("disable: status=%d body=%s", disable.Code, disable.Body.String())
	}

	recorder := httptest.NewRecorder()
	testHandler.InvokePluginHook(recorder, invokeHookRequest(installationID, "summarize", map[string]any{"trigger": "manual"}))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s, want 403", recorder.Code, recorder.Body.String())
	}
}

// The install token is returned once and stored only as a hash, so the same
// token must never come back from a second rotation, and the old one must stop
// working the moment a new one is issued.
func TestRotatePluginTokenIssuesOnceAndInvalidatesThePrevious(t *testing.T) {
	withPluginsV1Flag(t, testHandler, true)
	cleanupPluginInstallations(t)
	installationID := installHookPlugin(t)
	testHandler.PluginService.DeploymentKey = bytes.Repeat([]byte{9}, 32)
	t.Cleanup(func() { testHandler.PluginService.DeploymentKey = nil })

	first := rotateToken(t, installationID)
	if first.Token == "" || first.SigningSecret == "" {
		t.Fatalf("rotation returned nothing usable: %+v", first)
	}

	installation, err := testHandler.PluginService.AuthenticateInstallToken(context.Background(), first.Token)
	if err != nil {
		t.Fatalf("the freshly issued token must authenticate: %v", err)
	}
	if uuidToString(installation.ID) != installationID {
		t.Fatalf("token resolved to the wrong installation: %s", uuidToString(installation.ID))
	}

	second := rotateToken(t, installationID)
	if second.Token == first.Token {
		t.Fatal("rotation reissued the same token")
	}
	if _, err := testHandler.PluginService.AuthenticateInstallToken(context.Background(), first.Token); err == nil {
		t.Fatal("the previous token must stop working after a rotation")
	}
	// Derived from the deployment key and the installation, so it survives
	// rotation — an author does not have to reconfigure their server every
	// time an admin rotates the token.
	if second.SigningSecret != first.SigningSecret {
		t.Fatal("the signing secret must be stable across token rotations")
	}
}

func TestRevokePluginTokenStopsAuthentication(t *testing.T) {
	withPluginsV1Flag(t, testHandler, true)
	cleanupPluginInstallations(t)
	installationID := installHookPlugin(t)
	testHandler.PluginService.DeploymentKey = bytes.Repeat([]byte{9}, 32)
	t.Cleanup(func() { testHandler.PluginService.DeploymentKey = nil })

	issued := rotateToken(t, installationID)
	recorder := httptest.NewRecorder()
	testHandler.RevokePluginToken(recorder, pluginHandlerRequest(http.MethodDelete, "/token", nil, map[string]string{
		"id": testWorkspaceID, "installationId": installationID,
	}))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("revoke: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if _, err := testHandler.PluginService.AuthenticateInstallToken(context.Background(), issued.Token); err == nil {
		t.Fatal("a revoked token must not authenticate")
	}
}

// A garbage token must not resolve to some installation by accident — the
// lookup is by hash, so an empty or malformed value must be refused before it
// reaches the query.
func TestAuthenticateInstallTokenRefusesMalformedValues(t *testing.T) {
	for _, token := range []string{"", "mpi_", "not-a-token", "mpc_something", "Bearer mpi_x"} {
		if _, err := testHandler.PluginService.AuthenticateInstallToken(context.Background(), token); err == nil {
			t.Fatalf("token %q must not authenticate", token)
		}
	}
}

func rotateToken(t *testing.T, installationID string) pluginTokenResponse {
	t.Helper()
	recorder := httptest.NewRecorder()
	testHandler.RotatePluginToken(recorder, pluginHandlerRequest(http.MethodPost, "/token", nil, map[string]string{
		"id": testWorkspaceID, "installationId": installationID,
	}))
	if recorder.Code != http.StatusOK {
		t.Fatalf("rotate: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var issued pluginTokenResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &issued); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	return issued
}
