package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

// hermesACPProviderUnconfiguredScript is a `hermes acp` stand-in that completes
// initialize and then rejects session/new exactly as hermes 0.20.0 does when it
// resolves no LLM provider — verified against the real binary by pointing it at
// an empty HERMES_HOME. The error frame carries its message in a `data` OBJECT,
// not a string, which is why the transport renders it as raw JSON and why
// ProviderUnconfigured matches on a substring.
func hermesACPProviderUnconfiguredScript() string {
	// hermes' message quotes its own commands in backticks, which cannot
	// appear inside a Go raw string literal — hence the concatenation. Inside
	// the shell script they sit in single quotes, so sh treats them as text
	// rather than command substitution.
	bt := "`"
	details := "No LLM provider configured. Run " + bt + "hermes model" + bt +
		" to select a provider, or run " + bt + "hermes setup" + bt +
		" for first-time configuration."
	return `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"hermes-agent","version":"0.20.0"}}}\n' "$id"
      ;;
    *'"method":"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"Internal error","data":{"details":"` + details + `"}}}\n' "$id"
      ;;
  esac
done
`
}

// hermesACPHandshakeErrorScript fails session/new for a reason that has nothing
// to do with configuration.
func hermesACPHandshakeErrorScript() string {
	return `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{}}}\n' "$id"
      ;;
    *'"method":"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"Internal error","data":{"details":"401 invalid api key"}}}\n' "$id"
      ;;
  esac
done
`
}

func hermesACPCatalogScript() string {
	return `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{}}}\n' "$id"
      ;;
    *'"method":"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"ses_ok","models":{"currentModelId":"openai-codex:gpt-5.6-terra","availableModels":[{"modelId":"openai-codex:gpt-5.6-terra","name":"OpenAI Codex · gpt-5.6-terra"},{"modelId":"openai-codex:gpt-5.5","name":"OpenAI Codex · gpt-5.5"}]}}}\n' "$id"
      ;;
  esac
done
`
}

// TestDiscoverHermesModelsSurfacesSessionNewFailure is the regression this file
// exists for (MUL-6606). Hermes ships no static catalog, so swallowing a
// session/new failure into an empty list reports "discovery succeeded and found
// nothing" — which the picker renders as an authoritative empty dropdown with
// no error, no reason, and no prompt to type a model in by hand. The failure
// must reach the caller so the daemon reports status=failed.
func TestDiscoverHermesModelsSurfacesSessionNewFailure(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		script string
		// wantHint is true only for the provider-unconfigured failure; every
		// other failure must pass through with its own text and nothing added.
		wantHint bool
	}{
		{name: "provider unconfigured", script: hermesACPProviderUnconfiguredScript(), wantHint: true},
		{name: "rejected credential", script: hermesACPHandshakeErrorScript(), wantHint: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fakePath := filepath.Join(t.TempDir(), "hermes")
			writeTestExecutable(t, fakePath, []byte(tc.script))

			models, err := discoverHermesModels(context.Background(), Command{Path: fakePath})
			if err == nil {
				t.Fatalf("session/new failed but discovery reported success with %d models", len(models))
			}
			if len(models) != 0 {
				t.Errorf("a failed discovery must return no models, got %+v", models)
			}
			// The runtime's own words are the actionable part of the message;
			// the wrapper must not replace them.
			if !strings.Contains(err.Error(), "session/new") {
				t.Errorf("error must name the stage that failed, got: %v", err)
			}
			if got := strings.Contains(err.Error(), hermesDiscoveryUnconfiguredHint); got != tc.wantHint {
				t.Errorf("hint present=%v, want %v; error: %v", got, tc.wantHint, err)
			}
		})
	}
}

// TestDiscoverHermesModelsUnconfiguredHintPointsAtTheDaemonEnvironment pins the
// content of the hint, not just its presence. Discovery builds no per-task
// HERMES_HOME overlay and never reads an agent's custom_env, so the task path's
// advice (annotateHermesProviderUnconfigured in internal/daemon) is not merely
// unhelpful here — following it cannot put a single model in the picker. The
// hint has to send the reader at the daemon's own environment instead.
func TestDiscoverHermesModelsUnconfiguredHintPointsAtTheDaemonEnvironment(t *testing.T) {
	t.Parallel()

	fakePath := filepath.Join(t.TempDir(), "hermes")
	writeTestExecutable(t, fakePath, []byte(hermesACPProviderUnconfiguredScript()))

	_, err := discoverHermesModels(context.Background(), Command{Path: fakePath})
	if err == nil {
		t.Fatal("expected the unconfigured-provider failure to propagate")
	}
	msg := err.Error()

	// Hermes' own message survives: it is what the runtime actually said.
	if !strings.Contains(msg, "No LLM provider configured") {
		t.Errorf("the runtime's own message must be preserved, got: %s", msg)
	}
	// And the hint carries what hermes could not know — that it was answering
	// the daemon, not the user's shell — plus a check the user can run.
	for _, want := range []string{"daemon", "login shell", "env -i"} {
		if !strings.Contains(msg, want) {
			t.Errorf("hint must mention %q, got: %s", want, msg)
		}
	}
	// The task path's remedy must not leak in: it would send the reader to
	// edit config that discovery provably never reads.
	if !strings.Contains(hermesDiscoveryUnconfiguredHint, "will not populate this picker") {
		t.Error("hint must say outright that HERMES_HOME / custom_env cannot fix the picker")
	}
}

// TestAnnotateHermesDiscoveryUnconfiguredLeavesOtherFailuresAlone covers the
// stages a fake ACP binary cannot reach — a missing binary never speaks the
// protocol at all — and proves the predicate, not the transport, is the gate.
func TestAnnotateHermesDiscoveryUnconfiguredLeavesOtherFailuresAlone(t *testing.T) {
	t.Parallel()

	if got := annotateHermesDiscoveryUnconfigured(nil); got != nil {
		t.Errorf("nil error must stay nil, got: %v", got)
	}
	for _, msg := range []string{
		`ACP model discovery executable lookup failed: exec: "hermes": executable file not found in $PATH`,
		"ACP model discovery initialize failed: unexpected EOF",
		"ACP model discovery session/new failed: session/new: Internal error (code=-32603, data=401 invalid api key)",
		"ACP model discovery completion failed: context deadline exceeded",
	} {
		in := errString(msg)
		if got := annotateHermesDiscoveryUnconfigured(in); got.Error() != msg {
			t.Errorf("error text must be untouched\n got: %s\nwant: %s", got.Error(), msg)
		}
	}
}

// TestHermesDiscoveryHintDoesNotReclassifyTheFailure keeps the hint inert. The
// same predicate that gates it (taskfailure.ProviderUnconfigured) also feeds
// Classify, and this text is appended to an error string other code reads —
// so it must not change what that error is understood to be.
func TestHermesDiscoveryHintDoesNotReclassifyTheFailure(t *testing.T) {
	t.Parallel()

	for _, msg := range []string{
		`hermes session/new failed: session/new: Internal error (code=-32603, data={"details":"No LLM provider configured."})`,
		"ACP model discovery initialize failed: unexpected EOF",
		"API Error: prompt is too long: 250000 tokens > 200000 maximum",
	} {
		annotated := msg + hermesDiscoveryUnconfiguredHint
		if got, want := taskfailure.Classify(annotated), taskfailure.Classify(msg); got != want {
			t.Errorf("hint moved the failure reason for %q: %q -> %q", msg, want, got)
		}
	}
}

// TestDiscoverHermesModelsStillReturnsACatalogOnSuccess guards the other half:
// strictErrors must not have turned a working hermes into a failing one.
func TestDiscoverHermesModelsStillReturnsACatalogOnSuccess(t *testing.T) {
	t.Parallel()

	fakePath := filepath.Join(t.TempDir(), "hermes")
	writeTestExecutable(t, fakePath, []byte(hermesACPCatalogScript()))

	models, err := discoverHermesModels(context.Background(), Command{Path: fakePath})
	if err != nil {
		t.Fatalf("discover hermes models: %v", err)
	}
	if len(models) != 2 || models[0].ID != "openai-codex:gpt-5.6-terra" {
		t.Fatalf("unexpected models: %+v", models)
	}
	if !models[0].Default {
		t.Error("currentModelId must still be badged as the default pick")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// TestACPDiscoveryTimeoutIsPerProvider covers the knob hermes needs: a provider
// that sets no timeout keeps the shared default, and one that sets a short
// timeout is actually cut off by it.
func TestACPDiscoveryTimeoutIsPerProvider(t *testing.T) {
	t.Parallel()

	// A binary that never answers initialize. Without an honoured deadline
	// this test would hang rather than fail.
	stalled := `#!/bin/sh
while IFS= read -r line; do
  sleep 30
done
`
	fakePath := filepath.Join(t.TempDir(), "stalled")
	writeTestExecutable(t, fakePath, []byte(stalled))

	start := time.Now()
	_, err := discoverACPModels(context.Background(), Command{Path: fakePath}, acpDiscoveryProvider{
		defaultBin:   "stalled",
		clientName:   "multica-model-discovery",
		tmpdirPrefix: "multica-timeout-test-",
		strictErrors: true,
		timeout:      300 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected the per-provider deadline to abort the handshake")
	}
	// The override has to be what fired, not the 15s default.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("provider timeout was ignored; handshake ran for %s", elapsed)
	}
}

// TestHermesDiscoveryTimeoutCoversItsFailurePath is the reason the knob exists.
// Measured against hermes 0.20.0: a healthy session/new returns in ~2s, but a
// hermes whose configured provider cannot be resolved spends ~25s before
// reporting "No LLM provider configured" — the exact case MUL-6606 is about.
// Under the shared 15s default that diagnosis was replaced by "context deadline
// exceeded", so the budget must exceed the default by a real margin.
//
// The upper bound matters too: the server closes a claimed model-list request
// after 60s (handler.modelListRunningTimeout), and the daemon still needs a
// heartbeat pickup (up to 15s) inside that window. A discovery allowed to run
// past ~45s would report into a request record that no longer exists.
func TestHermesDiscoveryTimeoutCoversItsFailurePath(t *testing.T) {
	t.Parallel()

	if hermesDiscoveryTimeout <= acpDiscoveryDefaultTimeout {
		t.Fatalf("hermes budget %s must exceed the shared default %s; its failure path needs ~25s",
			hermesDiscoveryTimeout, acpDiscoveryDefaultTimeout)
	}
	if hermesDiscoveryTimeout < 30*time.Second {
		t.Errorf("hermes budget %s leaves no margin over the ~25s failure path", hermesDiscoveryTimeout)
	}
	if hermesDiscoveryTimeout > 45*time.Second {
		t.Errorf("hermes budget %s can outlive the server's 60s request window once heartbeat pickup is counted",
			hermesDiscoveryTimeout)
	}
}
