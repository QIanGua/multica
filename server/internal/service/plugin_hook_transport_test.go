package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/plugincontract"
)

// These exercise the real outbound path — a live HTTPS server, a real request,
// a real signature — rather than the pieces in isolation. The endpoint runs on
// loopback, which the SSRF guard refuses by design, so the test opts that exact
// origin in through DevOrigins: the same switch a plugin author uses to develop
// a hook against localhost. The consent check is NOT relaxed by it, which is
// what TestHookRefusesEndpointOutsideNetScope pins down.

type hookTestServer struct {
	server   *httptest.Server
	received chan hookReceivedRequest
	respond  func(w http.ResponseWriter, body []byte)
}

type hookReceivedRequest struct {
	Body      []byte
	Signature string
	Timestamp string
	Header    http.Header
}

func newHookTestServer(t *testing.T) *hookTestServer {
	t.Helper()
	harness := &hookTestServer{received: make(chan hookReceivedRequest, 8)}
	harness.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		harness.received <- hookReceivedRequest{
			Body:      body,
			Signature: r.Header.Get("X-Multica-Signature"),
			Timestamp: r.Header.Get("X-Multica-Timestamp"),
			Header:    r.Header.Clone(),
		}
		if harness.respond != nil {
			harness.respond(w, body)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(harness.server.Close)
	return harness
}

// hookTestService builds a service whose only outbound destination is the test
// server, with signing enabled.
func hookTestService(t *testing.T, harness *hookTestServer) *PluginService {
	t.Helper()
	service := testSigningService(t)
	service.DevOrigins = []string{harness.server.URL}
	service.HookClient = harness.server.Client()
	service.Callbacks = NewCallbackTokens()
	service.CallbackBaseURL = "https://multica.test/api/v1/plugin"
	return service
}

func hookTestInstallation(t *testing.T, endpoint, netScope string, triggers []string) db.PluginInstallation {
	t.Helper()
	triggerJSON, err := json.Marshal(triggers)
	if err != nil {
		t.Fatalf("marshal triggers: %v", err)
	}
	manifest := fmt.Sprintf(`{
		"manifest_version": 1,
		"key": "com.example.hooktest",
		"name": "Hook Test",
		"description": "d",
		"version": "1.0.0",
		"author": {"name": "example"},
		"scopes": ["issues:read", "net:%s"],
		"contributes": {"hooks": [{
			"key": "summarize",
			"name": "Summarize",
			"description": "Summarize the thread.",
			"triggers": %s,
			"events": ["issue.created"],
			"transport": {"type": "http", "url": "%s"}
		}]}
	}`, netScope, string(triggerJSON), endpoint)

	scopes, err := json.Marshal([]string{"issues:read", "net:" + netScope})
	if err != nil {
		t.Fatalf("marshal scopes: %v", err)
	}
	return db.PluginInstallation{
		ID:            testInstallationID(t),
		WorkspaceID:   testInstallationID(t),
		Enabled:       true,
		Manifest:      []byte(manifest),
		GrantedScopes: scopes,
	}
}

// The receiving end must be able to prove the call came from us, using only the
// signing secret it was configured with.
func TestHookRequestIsSignedAndVerifiableByTheReceiver(t *testing.T) {
	harness := newHookTestServer(t)
	service := hookTestService(t, harness)
	host := strings.TrimPrefix(harness.server.URL, "https://")
	host = strings.Split(host, ":")[0]
	installation := hookTestInstallation(t, harness.server.URL+"/hooks/summarize", host, []string{plugincontract.TriggerManual})

	hook, err := FindHook(installation, "summarize")
	if err != nil {
		t.Fatalf("find hook: %v", err)
	}
	output, err := service.callHookEndpoint(context.Background(), HookInvocation{
		Installation: installation,
		Hook:         hook,
		Trigger:      plugincontract.TriggerManual,
		Actor:        HookActor{Type: "member", ID: testInstallationID(t)},
		Input:        map[string]string{"issue_id": "abc"},
	})
	if err != nil {
		t.Fatalf("hook call: %v", err)
	}
	if string(output) != `{"ok":true}` {
		t.Fatalf("unexpected output %q", string(output))
	}

	select {
	case received := <-harness.received:
		secret, err := service.HookSigningSecret(installation.ID)
		if err != nil {
			t.Fatalf("derive secret: %v", err)
		}
		if err := VerifyHookSignature(secret, received.Timestamp, received.Body, received.Signature, time.Now()); err != nil {
			t.Fatalf("the receiver must be able to verify the signature: %v", err)
		}
		// One byte changed anywhere in the body must break it.
		tampered := append([]byte{}, received.Body...)
		tampered[len(tampered)-2] ^= 0xff
		if err := VerifyHookSignature(secret, received.Timestamp, tampered, received.Signature, time.Now()); err == nil {
			t.Fatal("a tampered body must not verify against the delivered signature")
		}
	default:
		t.Fatal("the endpoint received no request")
	}
}

// The `net:` scope is the whole promise made on the consent screen: this plugin
// sends data to these hosts and no others. DevOrigins must not widen it.
func TestHookRefusesEndpointOutsideNetScope(t *testing.T) {
	harness := newHookTestServer(t)
	service := hookTestService(t, harness)
	// Granted somewhere else entirely, while the transport points at the test
	// server the operator did opt in to.
	installation := hookTestInstallation(t, harness.server.URL+"/hooks/summarize", "elsewhere.example.com", []string{plugincontract.TriggerManual})

	hook, err := FindHook(installation, "summarize")
	if err != nil {
		t.Fatalf("find hook: %v", err)
	}
	_, err = service.callHookEndpoint(context.Background(), HookInvocation{
		Installation: installation,
		Hook:         hook,
		Trigger:      plugincontract.TriggerManual,
	})
	if err == nil {
		t.Fatal("a destination outside the granted net: scope must be refused")
	}
	if !strings.Contains(err.Error(), "net:") {
		t.Fatalf("the refusal should name the scope, got %v", err)
	}
	select {
	case <-harness.received:
		t.Fatal("no request may leave for an unapproved host")
	default:
	}
}

// An installation with no net: scope cannot call out at all.
func TestHookRefusesWhenNoNetScopeGranted(t *testing.T) {
	harness := newHookTestServer(t)
	service := hookTestService(t, harness)
	installation := hookTestInstallation(t, harness.server.URL+"/hooks/summarize", "example.com", []string{plugincontract.TriggerManual})
	installation.GrantedScopes = []byte(`["issues:read"]`)

	hook, err := FindHook(installation, "summarize")
	if err != nil {
		t.Fatalf("find hook: %v", err)
	}
	if _, err := service.callHookEndpoint(context.Background(), HookInvocation{
		Installation: installation,
		Hook:         hook,
		Trigger:      plugincontract.TriggerManual,
	}); err == nil {
		t.Fatal("an installation with no net: scope must not reach the network")
	}
}

// The callback token travels in the body, so a handler can answer without ever
// being given standing access — and it redeems exactly once.
func TestHookCarriesAOneShotCallbackToken(t *testing.T) {
	harness := newHookTestServer(t)
	service := hookTestService(t, harness)
	host := strings.Split(strings.TrimPrefix(harness.server.URL, "https://"), ":")[0]
	installation := hookTestInstallation(t, harness.server.URL+"/hooks/summarize", host, []string{plugincontract.TriggerManual})

	hook, err := FindHook(installation, "summarize")
	if err != nil {
		t.Fatalf("find hook: %v", err)
	}
	actorID := testInstallationID(t)
	if _, err := service.callHookEndpoint(context.Background(), HookInvocation{
		Installation: installation,
		Hook:         hook,
		Trigger:      plugincontract.TriggerManual,
		Actor:        HookActor{Type: "member", ID: actorID},
	}); err != nil {
		t.Fatalf("hook call: %v", err)
	}

	received := <-harness.received
	var body hookRequestBody
	if err := json.Unmarshal(received.Body, &body); err != nil {
		t.Fatalf("decode delivered body: %v", err)
	}
	if body.CallbackToken == "" {
		t.Fatal("the handler needs a callback token to answer with")
	}
	if body.CallbackURL != "https://multica.test/api/v1/plugin" {
		t.Fatalf("unexpected callback url %q", body.CallbackURL)
	}
	if body.Actor.Type != "member" {
		t.Fatalf("a manual trigger must carry the member actor, got %q", body.Actor.Type)
	}

	grant, err := service.Callbacks.Redeem(body.CallbackToken)
	if err != nil {
		t.Fatalf("first redemption must succeed: %v", err)
	}
	if grant.Actor.Type != "member" || uuidString(grant.Actor.ID) != uuidString(actorID) {
		t.Fatalf("the grant must carry the actor decided at dispatch, got %+v", grant.Actor)
	}
	if _, err := service.Callbacks.Redeem(body.CallbackToken); err == nil {
		t.Fatal("a callback token must not redeem twice")
	}
}

// A hook may not be reached through a trigger it never declared, even if the
// caller asks for one the host supports.
func TestInvokeHookRefusesUndeclaredTrigger(t *testing.T) {
	harness := newHookTestServer(t)
	service := hookTestService(t, harness)
	host := strings.Split(strings.TrimPrefix(harness.server.URL, "https://"), ":")[0]
	installation := hookTestInstallation(t, harness.server.URL+"/hooks/summarize", host, []string{plugincontract.TriggerManual})

	hook, err := FindHook(installation, "summarize")
	if err != nil {
		t.Fatalf("find hook: %v", err)
	}
	if _, err := service.InvokeHook(context.Background(), HookInvocation{
		Installation: installation,
		Hook:         hook,
		Trigger:      plugincontract.TriggerEvent,
	}, 1); err == nil {
		t.Fatal("the event trigger was not declared and must be refused")
	}
	select {
	case <-harness.received:
		t.Fatal("no request may leave for an undeclared trigger")
	default:
	}
}

// A disabled installation is off, not merely hidden.
func TestInvokeHookRefusesDisabledInstallation(t *testing.T) {
	harness := newHookTestServer(t)
	service := hookTestService(t, harness)
	host := strings.Split(strings.TrimPrefix(harness.server.URL, "https://"), ":")[0]
	installation := hookTestInstallation(t, harness.server.URL+"/hooks/summarize", host, []string{plugincontract.TriggerManual})
	installation.Enabled = false

	hook, err := FindHook(installation, "summarize")
	if err != nil {
		t.Fatalf("find hook: %v", err)
	}
	if _, err := service.InvokeHook(context.Background(), HookInvocation{
		Installation: installation,
		Hook:         hook,
		Trigger:      plugincontract.TriggerManual,
	}, 1); err == nil {
		t.Fatal("a disabled installation must not call out")
	}
}

// A failing endpoint must produce an error the caller can act on, and must not
// leak whatever the remote end said into our records.
func TestHookFailureIsRedactedAndClassified(t *testing.T) {
	harness := newHookTestServer(t)
	harness.respond = func(w http.ResponseWriter, _ []byte) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal detail: secret-token-abc123 and an issue title"))
	}
	service := hookTestService(t, harness)
	host := strings.Split(strings.TrimPrefix(harness.server.URL, "https://"), ":")[0]
	installation := hookTestInstallation(t, harness.server.URL+"/hooks/summarize", host, []string{plugincontract.TriggerManual})

	hook, err := FindHook(installation, "summarize")
	if err != nil {
		t.Fatalf("find hook: %v", err)
	}
	_, err = service.callHookEndpoint(context.Background(), HookInvocation{
		Installation: installation,
		Hook:         hook,
		Trigger:      plugincontract.TriggerManual,
	})
	if err == nil {
		t.Fatal("a 500 from the endpoint must be an error")
	}
	message := redactHookError(err)
	if strings.Contains(message, "secret-token-abc123") || strings.Contains(message, "issue title") {
		t.Fatalf("the recorded error must not echo the remote body, got %q", message)
	}
	if hookFailureStatus(err) != "failed" {
		t.Fatalf("an unreachable-or-erroring endpoint is 'failed', got %q", hookFailureStatus(err))
	}
}

// Refusals are decisions, not outages. The dispatcher relies on this to avoid
// retrying a call that will be refused identically three times.
func TestHookFailureStatusSeparatesRefusalsFromOutages(t *testing.T) {
	refused := pluginErrf(PluginErrorForbidden, "not granted")
	if hookFailureStatus(refused) != "refused" {
		t.Fatalf("a forbidden error is a refusal, got %q", hookFailureStatus(refused))
	}
	quota := pluginErrf(PluginErrorQuota, "too many")
	if hookFailureStatus(quota) != "refused" {
		t.Fatalf("a quota error is a refusal, got %q", hookFailureStatus(quota))
	}
	outage := &PluginError{Kind: PluginErrorUnavailable, Message: "endpoint did not answer"}
	if hookFailureStatus(outage) != "failed" {
		t.Fatalf("an unavailable endpoint is a failure, got %q", hookFailureStatus(outage))
	}
}
