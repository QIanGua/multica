//go:build !windows

package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// These cases need a POSIX shell stub, or the CodeBuddy session capture in
// codebuddy_discovery_fallback_test.go, which is itself !windows. The
// provider-neutral parser/apply tests stay in acp_effort_test.go so they run
// on every platform.

// ── CodeBuddy overlay regression ─────────────────────────────────────

// TestCodebuddyOverlaySurvivesGeneralization: CodeBuddy applies the level
// through `--effort`, not over ACP, so its narrower flag vocabulary must keep
// filtering the advertised list even though the parser underneath is shared.
func TestCodebuddyOverlaySurvivesGeneralization(t *testing.T) {
	t.Parallel()
	levels, def := parseACPCodebuddyEffort(json.RawMessage(codebuddyACPSessionResult))
	if strings.Join(levels, ",") != "minimal,low,medium,high,xhigh,max" {
		t.Errorf("levels = %v, want the six --effort values without `enabled`", levels)
	}
	if def != "" {
		t.Errorf("DefaultLevel = %q, want empty because currentValue was the unusable `enabled`", def)
	}

	// The same payload through the generic path keeps `enabled`: the overlay is
	// CodeBuddy's, not everyone's.
	option, ok := parseACPEffortOption(json.RawMessage(codebuddyACPSessionResult))
	if !ok || !option.supports("enabled") {
		t.Error("the shared parser must not apply CodeBuddy's flag whitelist")
	}
}

// ── End-to-end discovery ─────────────────────────────────────────────

func reasonixEffortDiscoveryStub() string {
	return `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*) printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{}}}\n' "$id" ;;
    *'"method":"session/new"'*) printf '{"jsonrpc":"2.0","id":%s,"result":` + reasonixEffortSessionResult + `}\n' "$id" ;;
  esac
done
`
}

// TestValidateThinkingLevelReasonix covers the daemon's pre-execution guard.
// It fails closed on an unknown (provider, model, level), so without a
// discovered catalog it would silently drop every level and the feature would
// look wired up while doing nothing.
func TestValidateThinkingLevelReasonix(t *testing.T) {
	fakePath := filepath.Join(t.TempDir(), "reasonix")
	writeTestExecutable(t, fakePath, []byte(reasonixEffortDiscoveryStub()))

	for _, tc := range []struct {
		model string
		level string
		want  bool
	}{
		{model: "deepseek-v4-flash", level: "max", want: true},
		// `auto` and `disabled` only survive because the shared parser takes
		// the advertised list verbatim; CodeBuddy's whitelist would drop them.
		{model: "deepseek-v4-flash", level: "auto", want: true},
		{model: "deepseek-v4-flash", level: "disabled", want: true},
		{model: "deepseek-v4-flash", level: "ultra", want: false},
		// Empty model resolves to the catalog's Default (the advertised
		// currentModelId), so a default-model agent still gets its level.
		{model: "", level: "high", want: true},
		{model: "no-such-model", level: "high", want: false},
	} {
		got, err := ValidateThinkingLevel(context.Background(), "reasonix", fakePath, tc.model, tc.level)
		if err != nil {
			t.Fatalf("ValidateThinkingLevel(%q, %q): %v", tc.model, tc.level, err)
		}
		if got != tc.want {
			t.Errorf("ValidateThinkingLevel(%q, %q) = %v, want %v", tc.model, tc.level, got, tc.want)
		}
	}
}

// TestDiscoverReasonixModelsAnnotatesEffort proves the wiring: one ACP
// handshake yields both the model catalog and the effort catalog, with no
// reasonix-specific parsing anywhere in the path.
func TestDiscoverReasonixModelsAnnotatesEffort(t *testing.T) {
	fakePath := filepath.Join(t.TempDir(), "reasonix")
	writeTestExecutable(t, fakePath, []byte(reasonixEffortDiscoveryStub()))

	models, err := discoverReasonixModels(context.Background(), fakePath)
	if err != nil {
		t.Fatalf("discoverReasonixModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %+v, want 2", models)
	}
	for _, m := range models {
		if m.Thinking == nil {
			t.Fatalf("model %q carries no effort catalog", m.ID)
		}
		if len(m.Thinking.SupportedLevels) != 5 || m.Thinking.DefaultLevel != "max" {
			t.Errorf("model %q: thinking = %+v", m.ID, m.Thinking)
		}
	}
}
