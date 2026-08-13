package daemon

import (
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

// hermesProviderUnconfiguredError is the failure exactly as it reaches the
// daemon: Hermes' message wrapped in the ACP transport's framing, which is why
// the predicate matches on a substring rather than equality.
const hermesProviderUnconfiguredError = `hermes session/new failed: session/new: Internal error ` +
	`(code=-32603, data={"details":"No LLM provider configured. Run ` + "`hermes model`" +
	` to select a provider, or run ` + "`hermes setup`" + ` for first-time configuration."})`

func TestAnnotateHermesProviderUnconfigured(t *testing.T) {
	t.Parallel()

	const overlay = "/home/u/multica_workspaces/ws/abc123/hermes-home"
	const source = "/home/u/.hermes"

	t.Run("names both homes", func(t *testing.T) {
		t.Parallel()
		got := annotateHermesProviderUnconfigured(hermesProviderUnconfiguredError, "hermes", overlay, source)

		// The original text has to survive: it is what the runtime actually
		// said, and the annotation is additive context, not a replacement.
		if !strings.Contains(got, "No LLM provider configured") {
			t.Errorf("the runtime's own message must be preserved, got: %s", got)
		}
		// Both paths, because they answer different questions: the overlay is
		// what Hermes read, the source home is where the user's config was
		// expected to be. Reporting only one leaves the user guessing again.
		if !strings.Contains(got, overlay) {
			t.Errorf("annotation must name the overlay HERMES_HOME, got: %s", got)
		}
		if !strings.Contains(got, source) {
			t.Errorf("annotation must name the source home it was seeded from, got: %s", got)
		}
		if !strings.Contains(got, "custom_env") {
			t.Errorf("annotation must name the actionable remedy, got: %s", got)
		}
	})

	t.Run("leaves unrelated failures alone", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name     string
			errMsg   string
			provider string
			overlay  string
		}{
			// A different provider's error can carry similar words; the
			// HERMES_HOME advice would be nonsense there.
			{"other provider", hermesProviderUnconfiguredError, "codex", overlay},
			// No overlay was built, so there is no seeding story to tell.
			{"no overlay", hermesProviderUnconfiguredError, "hermes", ""},
			// An expired or rejected credential is a different failure: the
			// config WAS found. Widening into it would send users to the wrong
			// fix, which is the very thing this annotation exists to stop.
			{"auth failure", "hermes session/prompt failed: 401 invalid api key", "hermes", overlay},
			{"empty", "", "hermes", overlay},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				if got := annotateHermesProviderUnconfigured(tc.errMsg, tc.provider, tc.overlay, source); got != tc.errMsg {
					t.Errorf("error text must be untouched, got: %s", got)
				}
			})
		}
	})
}

// TestAnnotationIsClassifierRelevantHenceAnnotatedLast is why the call site
// annotates only after every classifier has read errMsg.
//
// The annotation embeds two filesystem paths the user chose, and Classify
// matches substrings anywhere in the text — so a directory name can satisfy a
// rule. Rule 1 (context overflow) keys on "token" AND "limit" and is evaluated
// before rule 2 (missing config), so a home under /srv/token-cache/limit/ is
// enough to relabel this failure. That is not cosmetic: failure_reason drives
// retry eligibility and resume blacklisting.
//
// The ordering is the fix. This test pins the hazard the ordering protects
// against, so anyone tempted to annotate earlier sees the cost first.
func TestAnnotationIsClassifierRelevantHenceAnnotatedLast(t *testing.T) {
	t.Parallel()

	unannotated := taskfailure.Classify(hermesProviderUnconfiguredError)
	if unannotated != taskfailure.ReasonAgentMissingConfig {
		t.Fatalf("precondition: this failure should classify as missing_config, got %q", unannotated)
	}

	// Ordinary paths: nothing in the annotation is classifier-relevant, so the
	// common case would be safe even if it were classified.
	benign := annotateHermesProviderUnconfigured(hermesProviderUnconfiguredError, "hermes",
		"/home/u/multica_workspaces/ws/abc123/hermes-home", "/home/u/.hermes")
	if got := taskfailure.Classify(benign); got != unannotated {
		t.Errorf("ordinary paths should not perturb classification: %q -> %q", unannotated, got)
	}

	// Adversarial-but-legal paths: they do perturb it. If this assertion ever
	// starts failing, Classify's rules changed such that annotation text is no
	// longer classifier-relevant — re-read the ordering comment at the call
	// site before relying on that.
	hostile := annotateHermesProviderUnconfigured(hermesProviderUnconfiguredError, "hermes",
		"/srv/token-cache/limit/hermes-home", "/srv/token-cache/limit/.hermes")
	if got := taskfailure.Classify(hostile); got == unannotated {
		t.Errorf("expected annotation text to be able to perturb classification (that is why it runs last), but %q survived", got)
	}
}
