package llm

import (
	"context"
	"io"
	"net/http"
	"testing"

	openai "github.com/openai/openai-go/v3"
)

// retryingUpstream always fails with a status the SDK retries, and counts how
// many requests it saw. `Retry-After: 0` keeps the assertions about the retry
// COUNT from also paying for the backoff curve — the SDK honors the header
// verbatim, so the test runs in milliseconds instead of seconds.
func retryingUpstream(t *testing.T, count *int) string {
	t.Helper()
	return stubUpstream(t, func(w http.ResponseWriter, _ map[string]any) {
		*count++
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom","type":"server_error"}}`)
	}).URL
}

// TestMaxRetriesSemantics pins the four supported states of Config.MaxRetries
// against the only thing that matters to an operator: how many times we hit
// their upstream. Before MUL-6364 an explicit 0 landed in the "unset" row here
// and a negative was the only way to reach the "disabled" one.
func TestMaxRetriesSemantics(t *testing.T) {
	for _, tc := range []struct {
		name         string
		configured   *int
		wantRequests int
		wantBudget   RetryBudget
	}{
		{
			name:         "unset uses the documented default",
			configured:   nil,
			wantRequests: DefaultMaxRetries + 1,
			wantBudget:   RetryBudget{MaxRetries: DefaultMaxRetries, Source: RetrySourceDefault, RequestTimeout: defaultRequestTimeout},
		},
		{
			name:         "zero disables retries",
			configured:   retries(0),
			wantRequests: 1,
			wantBudget:   RetryBudget{MaxRetries: 0, Source: RetrySourceConfig, RequestTimeout: defaultRequestTimeout},
		},
		{
			name:         "one retries exactly once",
			configured:   retries(1),
			wantRequests: 2,
			wantBudget:   RetryBudget{MaxRetries: 1, Source: RetrySourceConfig, RequestTimeout: defaultRequestTimeout},
		},
		{
			name:         "positive value is honored exactly",
			configured:   retries(5),
			wantRequests: 6,
			wantBudget:   RetryBudget{MaxRetries: 5, Source: RetrySourceConfig, RequestTimeout: defaultRequestTimeout},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			c := New(Config{APIKey: "k", BaseURL: retryingUpstream(t, &requests), MaxRetries: tc.configured})

			if got := c.RetryBudget(); got != tc.wantBudget {
				t.Fatalf("RetryBudget() = %+v, want %+v", got, tc.wantBudget)
			}
			if _, err := c.Chat(context.Background(), openai.ChatCompletionNewParams{}); err == nil {
				t.Fatal("expected the upstream error to surface after the retry budget is spent")
			}
			if requests != tc.wantRequests {
				t.Fatalf("upstream saw %d requests, want %d", requests, tc.wantRequests)
			}
		})
	}
}

// TestNegativeMaxRetriesFallsBackToDefault covers the guard, not a supported
// input: cmd/server fails the boot on a negative before it can reach New. The
// guard exists because option.WithMaxRetries panics on a negative and New is
// documented never to fail, so the value must not be passed through — and it
// must not be quietly clamped to 0 either, which would read as "retries
// disabled" to anyone reading the startup diagnostic.
func TestNegativeMaxRetriesFallsBackToDefault(t *testing.T) {
	requests := 0
	c := New(Config{APIKey: "k", BaseURL: retryingUpstream(t, &requests), MaxRetries: retries(-1)})

	budget := c.RetryBudget()
	if budget.MaxRetries != DefaultMaxRetries || budget.Source != RetrySourceDefault {
		t.Fatalf("RetryBudget() = %+v, want the default budget from %q", budget, RetrySourceDefault)
	}
	if _, err := c.Chat(context.Background(), openai.ChatCompletionNewParams{}); err == nil {
		t.Fatal("expected the upstream error to surface")
	}
	if want := DefaultMaxRetries + 1; requests != want {
		t.Fatalf("upstream saw %d requests, want %d (default budget, not 0)", requests, want)
	}
}

// TestRetryBudgetOnDisabledClient guards the startup diagnostic: a deployment
// with no LLM configuration still logs one, so RetryBudget must be readable
// there rather than describing a client that will never dial anything.
func TestRetryBudgetOnDisabledClient(t *testing.T) {
	c := New(Config{})
	if c.Enabled() {
		t.Fatal("expected a disabled client")
	}
	if got := c.RetryBudget(); got.MaxRetries != DefaultMaxRetries || got.Source != RetrySourceDefault {
		t.Fatalf("RetryBudget() = %+v, want the default budget", got)
	}

	var nilClient *Client
	if got := (nilClient.RetryBudget()); got != (RetryBudget{}) {
		t.Fatalf("nil Client RetryBudget() = %+v, want the zero value", got)
	}
}

// TestMaxRetriesDoesNotBoundCompatibilityRetries keeps the two retry mechanisms
// in this package separable. The GenerateJSON parameter negotiation fires on a
// 400 the SDK never retries, so disabling the transport budget entirely must
// leave it working.
func TestMaxRetriesDoesNotBoundCompatibilityRetries(t *testing.T) {
	requests := 0
	srv := stubUpstream(t, func(w http.ResponseWriter, _ map[string]any) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		if requests == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Unsupported parameter: max_completion_tokens","type":"invalid_request_error","param":"max_completion_tokens","code":"unsupported_parameter"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"cmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"{}"},"finish_reason":"stop"}]}`)
	})

	c := New(Config{APIKey: "k", BaseURL: srv.URL, MaxRetries: retries(0)})
	if _, err := c.GenerateJSON(context.Background(), "legacy-model", "Return JSON.", "go", 0.3, 800); err != nil {
		t.Fatalf("GenerateJSON failed: %v", err)
	}
	if requests != 2 {
		t.Fatalf("upstream saw %d requests, want 2 (the negotiation runs with retries disabled)", requests)
	}
}
