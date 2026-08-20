package daemon

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/pkg/agent"
)

// stubModelDiscovery replaces the daemon's listModels indirection with a
// counting fake, so a test can assert BOTH what a runtime resolved to and
// whether discovery was reached at all. Counting the calls is the point: the
// cost of this lookup is a CLI subprocess with a 15-30s ceiling that is not
// memoized on failure, so "did not call" is a behaviour worth pinning.
func stubModelDiscovery(t *testing.T, catalogs map[string]agent.Catalog) func() int {
	t.Helper()
	var mu sync.Mutex
	calls := 0

	orig := listModels
	listModels = func(_ context.Context, provider string, _ agent.Command) (agent.Catalog, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return catalogs[provider], nil
	}
	t.Cleanup(func() { listModels = orig })

	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
}

func quietTaskLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestQualifyTaskModelSelectorsAndDiscoveryBoundary is the daemon-level
// orchestration contract for GH #7300: the runtimes whose catalogs are
// provider-namespaced get the selector their CLI actually accepts, and every
// runtime that could never benefit from the lookup never pays for it.
func TestQualifyTaskModelSelectorsAndDiscoveryBoundary(t *testing.T) {
	catalogs := map[string]agent.Catalog{
		"pi": {Models: []agent.Model{
			{ID: "multica-anthropic/claude/claude-opus-5", Provider: "multica-anthropic"},
		}},
		"opencode": {Models: []agent.Model{
			{ID: "multica-anthropic/claude/claude-opus-5", Provider: "multica-anthropic"},
			{ID: "multica-codex/codex/gpt-5.6-sol", Provider: "multica-codex"},
		}},
		// omp is a built-in runtime identity in the pi protocol family, so it
		// inherits the capability from its descriptor rather than a name check.
		"omp": {Models: []agent.Model{
			{ID: "anthropic/claude-sonnet-5", Provider: "anthropic"},
		}},
		// Present but unreachable: claude/codex must not get this far.
		"claude": {Models: []agent.Model{{ID: "claude-opus-5", Provider: "anthropic"}}},
		"codex":  {Models: []agent.Model{{ID: "gpt-5.6-sol", Provider: "openai"}}},
	}

	tests := []struct {
		name          string
		provider      string
		model         string
		want          string
		wantDiscovery bool
	}{
		{
			name:          "pi promotes a slash-shaped id to its catalog selector",
			provider:      "pi",
			model:         "claude/claude-opus-5",
			want:          "multica-anthropic/claude/claude-opus-5",
			wantDiscovery: true,
		},
		{
			name:          "opencode promotes the same id the CLI would reject bare",
			provider:      "opencode",
			model:         "claude/claude-opus-5",
			want:          "multica-anthropic/claude/claude-opus-5",
			wantDiscovery: true,
		},
		{
			name:          "omp inherits the capability from its protocol family",
			provider:      "omp",
			model:         "claude-sonnet-5",
			want:          "anthropic/claude-sonnet-5",
			wantDiscovery: true,
		},
		{
			name:          "an already-canonical selector survives untouched",
			provider:      "opencode",
			model:         "multica-codex/codex/gpt-5.6-sol",
			want:          "multica-codex/codex/gpt-5.6-sol",
			wantDiscovery: true,
		},
		{
			name:          "claude never reaches discovery",
			provider:      "claude",
			model:         "claude-opus-5",
			want:          "claude-opus-5",
			wantDiscovery: false,
		},
		{
			name:          "codex never reaches discovery",
			provider:      "codex",
			model:         "gpt-5.6-sol",
			want:          "gpt-5.6-sol",
			wantDiscovery: false,
		},
		{
			name:          "an empty model resolves at the runtime, not here",
			provider:      "opencode",
			model:         "",
			want:          "",
			wantDiscovery: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := stubModelDiscovery(t, catalogs)

			got := qualifyTaskModel(context.Background(), tt.provider, agent.Command{}, tt.model, quietTaskLog())
			if got != tt.want {
				t.Errorf("qualifyTaskModel(%s, %q) = %q, want %q", tt.provider, tt.model, got, tt.want)
			}
			if discovered := calls() > 0; discovered != tt.wantDiscovery {
				t.Errorf("discovery reached = %v, want %v (calls=%d)", discovered, tt.wantDiscovery, calls())
			}
		})
	}
}

// A runtime that cannot answer must not block the task: the persisted model
// may well be exactly what its CLI expects.
func TestQualifyTaskModelFailsOpenOnDiscoveryError(t *testing.T) {
	orig := listModels
	listModels = func(_ context.Context, _ string, _ agent.Command) (agent.Catalog, error) {
		return agent.Catalog{}, context.DeadlineExceeded
	}
	t.Cleanup(func() { listModels = orig })

	got := qualifyTaskModel(context.Background(), "opencode", agent.Command{}, "claude/claude-opus-5", quietTaskLog())
	if got != "claude/claude-opus-5" {
		t.Errorf("qualifyTaskModel on discovery error = %q, want the configured model unchanged", got)
	}
}
