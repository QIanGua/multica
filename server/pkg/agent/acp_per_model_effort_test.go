package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// reasonixModelSelectorSessionResult is a session/new response in the shape
// reasonix publishes its catalog through: a `model` config option carrying the
// available models, alongside the effort option for the model the session
// opened on. Per-model probing is only possible against this shape — the legacy
// `models.availableModels` block has no option id to address.
const reasonixModelSelectorSessionResult = `{"sessionId":"ses-reasonix",` +
	`"configOptions":[` +
	`{"type":"select","id":"model","name":"Model","category":"model","currentValue":"deepseek-v4-flash","options":[` +
	`{"value":"deepseek-v4-flash","name":"DeepSeek V4 Flash"},` +
	`{"value":"deepseek-v4-pro","name":"DeepSeek V4 Pro"},` +
	`{"value":"glm-5","name":"GLM 5"},` +
	`{"value":"local-none","name":"Local (no effort dial)"}]},` +
	`{"type":"select","id":"effort","name":"Effort","category":"thought_level","currentValue":"high","options":[` +
	`{"value":"auto","name":"Auto"},{"value":"disabled","name":"Disabled"},{"value":"low","name":"Low"},` +
	`{"value":"high","name":"High"},{"value":"max","name":"Max"}]}]}`

// switchResult builds the refreshed configOptions a runtime returns from
// session/set_config_option{model}: the model option now reporting `model` as
// its current value, plus that model's own effort vocabulary.
func switchResult(model string, effortValues ...string) json.RawMessage {
	options := ""
	for i, v := range effortValues {
		if i > 0 {
			options += ","
		}
		options += fmt.Sprintf(`{"value":%q,"name":%q}`, v, v)
	}
	effort := ""
	if len(effortValues) > 0 {
		effort = fmt.Sprintf(`,{"type":"select","id":"effort","category":"thought_level","currentValue":%q,"options":[%s]}`,
			effortValues[0], options)
	}
	return json.RawMessage(fmt.Sprintf(
		`{"configOptions":[{"type":"select","id":"model","category":"model","currentValue":%q,"options":[]}%s]}`,
		model, effort))
}

// modelsFromSelector is the catalog the shared parser produces for the session
// capture above, with the current model annotated exactly as discovery does.
func modelsFromSelector(t *testing.T) []Model {
	t.Helper()
	models := parseACPSessionNewModels(json.RawMessage(reasonixModelSelectorSessionResult))
	if len(models) != 4 {
		t.Fatalf("parsed %d models from the reasonix capture, want 4", len(models))
	}
	annotateACPThinkingForSessionModel(models, json.RawMessage(reasonixModelSelectorSessionResult))
	return models
}

// TestProbeACPPerModelEffortReadsPerModelCatalog is the whole point of the
// feature: every model ends up with the catalog the runtime reports for THAT
// model, not a copy of the session model's.
func TestProbeACPPerModelEffortReadsPerModelCatalog(t *testing.T) {
	t.Parallel()

	perModel := map[string][]string{
		// pro deliberately lacks `low` — the mismatch that made copying one
		// model's catalog onto its siblings a silent wrong answer (MUL-5991).
		"deepseek-v4-pro": {"auto", "disabled", "high", "max"},
		"glm-5":           {"auto", "enabled", "disabled"},
		"local-none":      nil, // protocol resolves to none: no effort option
	}
	var asked []string
	request := func(_ context.Context, method string, params any) (json.RawMessage, error) {
		if method != "session/set_config_option" {
			t.Fatalf("unexpected method %q", method)
		}
		p := params.(map[string]any)
		if p["configId"] != "model" {
			t.Fatalf("probe addressed config id %q, want \"model\"", p["configId"])
		}
		model := p["value"].(string)
		asked = append(asked, model)
		return switchResult(model, perModel[model]...), nil
	}

	models := modelsFromSelector(t)
	probeACPPerModelEffort(context.Background(), request, "reasonix", discardLogger(),
		"ses-reasonix", json.RawMessage(reasonixModelSelectorSessionResult), models, time.Minute)

	// The session's own model is never re-probed: its catalog arrived with the
	// handshake, so asking again would spend a session rebuild for nothing.
	want := []string{"deepseek-v4-pro", "glm-5", "local-none"}
	if len(asked) != len(want) {
		t.Fatalf("probed %v, want %v", asked, want)
	}
	for i := range want {
		if asked[i] != want[i] {
			t.Fatalf("probed %v, want %v", asked, want)
		}
	}

	byID := map[string]*ModelThinking{}
	for _, m := range models {
		byID[m.ID] = m.Thinking
	}
	assertLevels := func(model string, want []string) {
		t.Helper()
		got := thinkingValues(byID[model])
		if len(got) != len(want) {
			t.Fatalf("%s: levels %v, want %v", model, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: levels %v, want %v", model, got, want)
			}
		}
	}
	assertLevels("deepseek-v4-flash", []string{"auto", "disabled", "low", "high", "max"})
	assertLevels("deepseek-v4-pro", []string{"auto", "disabled", "high", "max"})
	assertLevels("glm-5", []string{"auto", "enabled", "disabled"})
	if byID["local-none"] != nil {
		t.Fatalf("local-none advertises no effort option; want nil Thinking, got %v",
			thinkingValues(byID["local-none"]))
	}
}

// TestProbeACPPerModelEffortRejectsUnconfirmedSwitch covers the failure this
// path is most exposed to: a runtime that accepts set_config_option and stays
// on the model it was already on. Attributing the response's effort option to
// the requested model would copy the session model's catalog onto every
// sibling — exactly the fake picker the confirmation exists to prevent.
func TestProbeACPPerModelEffortRejectsUnconfirmedSwitch(t *testing.T) {
	t.Parallel()

	request := func(_ context.Context, _ string, _ any) (json.RawMessage, error) {
		// Always answers with the session's original model, ignoring the ask.
		return switchResult("deepseek-v4-flash", "auto", "disabled", "low", "high", "max"), nil
	}

	models := modelsFromSelector(t)
	probeACPPerModelEffort(context.Background(), request, "reasonix", discardLogger(),
		"ses-reasonix", json.RawMessage(reasonixModelSelectorSessionResult), models, time.Minute)

	for _, m := range models {
		if m.ID == "deepseek-v4-flash" {
			continue // annotated from the handshake, not from a probe
		}
		if m.Thinking != nil {
			t.Fatalf("%s: unconfirmed switch produced a catalog %v; want nil",
				m.ID, thinkingValues(m.Thinking))
		}
	}
}

// TestProbeACPPerModelEffortStopsOnRequestError checks that a dead transport
// ends the sweep instead of repeating the same failure per model, and that
// models probed before the failure keep what they learned.
func TestProbeACPPerModelEffortStopsOnRequestError(t *testing.T) {
	t.Parallel()

	calls := 0
	request := func(_ context.Context, _ string, params any) (json.RawMessage, error) {
		calls++
		model := params.(map[string]any)["value"].(string)
		if calls == 1 {
			return switchResult(model, "auto", "high", "max"), nil
		}
		return nil, errors.New("reasonix process exited")
	}

	models := modelsFromSelector(t)
	probeACPPerModelEffort(context.Background(), request, "reasonix", discardLogger(),
		"ses-reasonix", json.RawMessage(reasonixModelSelectorSessionResult), models, time.Minute)

	if calls != 2 {
		t.Fatalf("made %d requests; want 2 (one success, then stop on the first error)", calls)
	}
	byID := map[string]*ModelThinking{}
	for _, m := range models {
		byID[m.ID] = m.Thinking
	}
	if got := thinkingValues(byID["deepseek-v4-pro"]); len(got) != 3 {
		t.Fatalf("deepseek-v4-pro lost the catalog it learned before the failure: %v", got)
	}
	if byID["glm-5"] != nil || byID["local-none"] != nil {
		t.Fatal("models after the transport failure must keep a nil Thinking")
	}
}

// TestProbeACPPerModelEffortRespectsBudget: an exhausted budget degrades to the
// pre-existing behaviour (no picker for the unprobed models) rather than
// blocking discovery or inventing a catalog.
func TestProbeACPPerModelEffortRespectsBudget(t *testing.T) {
	t.Parallel()

	calls := 0
	request := func(_ context.Context, _ string, params any) (json.RawMessage, error) {
		calls++
		return switchResult(params.(map[string]any)["value"].(string), "auto", "high"), nil
	}

	models := modelsFromSelector(t)
	probeACPPerModelEffort(context.Background(), request, "reasonix", discardLogger(),
		"ses-reasonix", json.RawMessage(reasonixModelSelectorSessionResult), models, -time.Second)

	if calls != 0 {
		t.Fatalf("made %d requests with an exhausted budget; want 0", calls)
	}
	for _, m := range models {
		if m.ID != "deepseek-v4-flash" && m.Thinking != nil {
			t.Fatalf("%s got a catalog despite the exhausted budget", m.ID)
		}
	}
}

// TestProbeACPPerModelEffortWithoutModelOptionIsNoOp guards the legacy ACP
// shape: a catalog advertised through `models.availableModels` carries no
// addressable model option, so there is nothing to drive and nothing to change.
func TestProbeACPPerModelEffortWithoutModelOptionIsNoOp(t *testing.T) {
	t.Parallel()

	request := func(_ context.Context, _ string, _ any) (json.RawMessage, error) {
		t.Fatal("probe issued a request against a session with no model config option")
		return nil, nil
	}

	models := parseACPSessionNewModels(json.RawMessage(reasonixEffortSessionResult))
	annotateACPThinkingForSessionModel(models, json.RawMessage(reasonixEffortSessionResult))
	probeACPPerModelEffort(context.Background(), request, "reasonix", discardLogger(),
		"ses-reasonix", json.RawMessage(reasonixEffortSessionResult), models, time.Minute)

	for _, m := range models {
		if m.ID == "deepseek-v4" && m.Thinking != nil {
			t.Fatal("non-session model gained a catalog with no model option to probe with")
		}
	}
}

func TestProbeACPPerModelEffortCancelledContextIsNoOp(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := func(_ context.Context, _ string, _ any) (json.RawMessage, error) {
		t.Fatal("probe issued a request under a cancelled context")
		return nil, nil
	}

	models := modelsFromSelector(t)
	probeACPPerModelEffort(ctx, request, "reasonix", discardLogger(),
		"ses-reasonix", json.RawMessage(reasonixModelSelectorSessionResult), models, time.Minute)
}

// ── acpModelConfigID ─────────────────────────────────────────────────

func TestAcpModelConfigID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload string
		want    string
		wantOK  bool
	}{
		{
			name:    "reasonix model option",
			payload: reasonixModelSelectorSessionResult,
			want:    "model",
			wantOK:  true,
		},
		{
			name:    "matched by category with a different id",
			payload: `{"configOptions":[{"id":"llm","category":"model","options":[]}]}`,
			want:    "llm",
			wantOK:  true,
		},
		{
			name:    "snake_case config options",
			payload: `{"config_options":[{"id":"model","options":[]}]}`,
			want:    "model",
			wantOK:  true,
		},
		{
			// Readable but not addressable: without an id there is nothing to
			// send back to session/set_config_option.
			name:    "model option without an id",
			payload: `{"configOptions":[{"id":"","category":"model","options":[]}]}`,
			wantOK:  false,
		},
		{
			name:    "legacy availableModels block",
			payload: reasonixEffortSessionResult,
			wantOK:  false,
		},
		{
			name:    "malformed payload",
			payload: `not json`,
			wantOK:  false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := acpModelConfigID(json.RawMessage(tc.payload))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Fatalf("config id = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestReasonixProbeBudgetFitsDiscoveryTimeout pins the invariant the constants
// carry: the sweep budget must leave the handshake room, so a switch that
// starts just under the budget can still finish instead of dying with the
// process.
func TestReasonixProbeBudgetFitsDiscoveryTimeout(t *testing.T) {
	t.Parallel()
	if reasonixEffortProbeBudget >= reasonixDiscoveryTimeout {
		t.Fatalf("probe budget %s must stay below the discovery timeout %s",
			reasonixEffortProbeBudget, reasonixDiscoveryTimeout)
	}
}

// TestACPDiscoveryTimeoutsFitClientPollWindow is the regression test for the
// cold-catalog case: a runtime with no cached catalog is discovered while a
// browser polls for the answer, and that poll gives up at
// modelDiscoveryClientPollTimeout. A discovery budget at or past it means the
// picker reports "model discovery timed out" while the daemon is still working
// — the user sees a failure and only the NEXT open benefits.
//
// Every provider timeout therefore has to leave room for the daemon to report
// on top of its own work. Raising one without checking this is the easy mistake
// (an earlier cut of the reasonix sweep used 45s against a 30s client cap).
func TestACPDiscoveryTimeoutsFitClientPollWindow(t *testing.T) {
	t.Parallel()

	// Room for the heartbeat pickup, the report round trip, and the client's
	// own 500ms poll interval.
	const reportingHeadroom = 5 * time.Second

	for name, timeout := range map[string]time.Duration{
		"default ACP handshake": acpDiscoveryTimeout,
		"reasonix":              reasonixDiscoveryTimeout,
	} {
		if timeout+reportingHeadroom > modelDiscoveryClientPollTimeout {
			t.Errorf("%s discovery timeout %s leaves under %s of headroom against the client's %s poll window",
				name, timeout, reportingHeadroom, modelDiscoveryClientPollTimeout)
		}
	}
}

// TestProbeACPPerModelEffortReservesTailOfDiscoveryWindow: the sweep must stop
// before the discovery context expires, so the catalog it built has time to be
// returned rather than dying with the context that was still probing. This
// holds even when the caller's budget is larger than the window that remains.
func TestProbeACPPerModelEffortReservesTailOfDiscoveryWindow(t *testing.T) {
	t.Parallel()

	// Less remaining than the reserve: nothing may be probed.
	ctx, cancel := context.WithTimeout(context.Background(), acpProbeCompletionReserve/2)
	defer cancel()

	calls := 0
	request := func(_ context.Context, _ string, params any) (json.RawMessage, error) {
		calls++
		return switchResult(params.(map[string]any)["value"].(string), "auto", "high"), nil
	}

	models := modelsFromSelector(t)
	probeACPPerModelEffort(ctx, request, "reasonix", discardLogger(),
		"ses-reasonix", json.RawMessage(reasonixModelSelectorSessionResult), models, time.Hour)

	if calls != 0 {
		t.Fatalf("made %d requests inside the completion reserve; want 0 even with a generous budget", calls)
	}
}
