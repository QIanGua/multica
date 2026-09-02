//go:build agentintegration

package agent

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestEnsureCodexRequestUserInputDisabledCompatibleWithCodexFeaturesList is the
// authorized live Codex CLI probe. Default tests keep deterministic parser and
// generated-config assertions; this binary-backed check requires
// MULTICA_RUN_REAL_AGENT_SMOKE=1.
func TestEnsureCodexRequestUserInputDisabledCompatibleWithCodexFeaturesList(t *testing.T) {
	requireRealAgentSmoke(t)

	codexPath, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex CLI not installed")
	}

	cases := []struct {
		name  string
		input string
	}{
		{
			name:  "bare inline under tools",
			input: "[tools]\nexperimental_request_user_input={enabled=true}\nweb_search = true\n",
		},
		{
			name:  "root inline table",
			input: "tools={experimental_request_user_input={enabled=true}, web_search=true}\n",
		},
		{
			name:  "quoted tools table",
			input: "[\"tools\"]\nexperimental_request_user_input={enabled=true}\n",
		},
		{
			name:  "quoted dotted table",
			input: "[\"tools\".\"experimental_request_user_input\"]\nenabled = true\n",
		},
		{
			name:  "quoted dotted key",
			input: "\"tools\".\"experimental_request_user_input\"={enabled=true}\n",
		},
		{
			name:  "quoted root inline",
			input: "\"tools\"={experimental_request_user_input={enabled=true}, web_search=true}\n",
		},
		{
			name:  "unicode escaped root inline key",
			input: `"\u0074ools"={experimental_request_user_input={enabled=true}, web_search=true}` + "\n",
		},
		{
			name:  "long unicode escaped root inline key",
			input: `"\U00000074ools"={experimental_request_user_input={enabled=true}, web_search=true}` + "\n",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			configPath := filepath.Join(home, "config.toml")
			if err := os.WriteFile(configPath, []byte(tc.input), 0o600); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if err := ensureCodexRequestUserInputDisabled(configPath, slog.Default()); err != nil {
				t.Fatalf("ensure: %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, codexPath, "features", "list")
			cmd.Env = append(os.Environ(), "CODEX_HOME="+home)
			out, err := cmd.CombinedOutput()
			got := string(out)
			lower := strings.ToLower(got)
			if strings.Contains(lower, "duplicate key") || strings.Contains(lower, "inline table") {
				t.Fatalf("pinned config must parse in Codex, output:\n%s", got)
			}
			if err != nil {
				t.Fatalf("codex features list failed: %v\n%s", err, got)
			}
		})
	}
}
