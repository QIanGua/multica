package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
)

// acp_effort.go holds the provider-neutral half of ACP reasoning-effort
// support: locating the effort selector a backend advertises in its
// `session/new` response, turning it into a model catalog, and pushing a
// persisted level back onto a live session.
//
// ACP gives us two generic primitives — `configOptions` on the session/new
// result and `session/set_config_option` — so discovery and application are
// the same shape for every backend. What the protocol does NOT standardise is
// the part that still needs convention here:
//
//   - Semantics. There is no "this option is reasoning effort" marker, so the
//     selector is matched by id/category against acpEffortOptionIDs.
//   - Vocabulary. reasonix advertises auto|disabled|low|high|max, Codex
//     none|minimal|…|ultra. We pass tokens through verbatim (MUL-2339) rather
//     than flattening them onto a shared enum.
//   - Whether setting it does anything. The protocol says a client MAY set an
//     option; it does not say the agent must act on it. Kimi ≤0.28.1 and
//     Hermes v0.18.2 both accept a level and leave the session unchanged
//     (see discoverKimiModels and ThinkingControlSupported). That is why
//     applyACPEffortOption reads the value back instead of assuming success,
//     and why a runtime only reaches this path once its Execute opts in.

// acpEffortOptionIDs are the option id / category tokens that mark a
// reasoning-effort selector. reasonix advertises id `effort` under category
// `thought_level`; CodeBuddy and jcode advertise `thought_level`. Matching
// either field keeps both shapes working without a per-runtime table.
var acpEffortOptionIDs = map[string]bool{
	"effort":        true,
	"thought_level": true,
}

// acpEffortOption is the effort selector one backend advertised.
type acpEffortOption struct {
	// ConfigID is what session/set_config_option must be given. It is NOT
	// interchangeable across backends — reasonix uses `effort`, CodeBuddy
	// `thought_level`, Kimi `thinking` — so it is read from the session rather
	// than hard-coded per runtime.
	ConfigID string
	// CurrentValue is the level the session starts on; "" when the backend
	// does not report one.
	CurrentValue string
	// Choices are the advertised levels in advertised order. Labels come from
	// the backend's own `name` field so the picker reads the way that CLI's
	// own UI does.
	Choices []ThinkingLevel
}

// supports reports whether value is one of the advertised choices.
func (o acpEffortOption) supports(value string) bool {
	for _, choice := range o.Choices {
		if choice.Value == value {
			return true
		}
	}
	return false
}

// values returns the advertised tokens, order preserved.
func (o acpEffortOption) values() []string {
	out := make([]string, 0, len(o.Choices))
	for _, choice := range o.Choices {
		out = append(out, choice.Value)
	}
	return out
}

// parseACPEffortOption extracts the effort selector from an ACP session/new (or
// session/resume) result. Reports false when the response carries no
// recognisable option, which every caller treats as "this runtime has no
// effort dial" rather than as an error.
//
// Values are taken verbatim: the CLI owns its vocabulary, and a whitelist here
// would silently drop levels a backend genuinely accepts. Callers that must
// narrow the set — CodeBuddy, whose `--effort` flag rejects a session-only
// choice the ACP surface still advertises — apply their own overlay on top.
func parseACPEffortOption(raw json.RawMessage) (acpEffortOption, bool) {
	type acpChoice struct {
		Value string `json:"value"`
		Name  string `json:"name"`
	}
	type acpOption struct {
		ID                string      `json:"id"`
		Category          string      `json:"category"`
		CurrentValue      string      `json:"currentValue"`
		CurrentValueSnake string      `json:"current_value"`
		Options           []acpChoice `json:"options"`
	}
	var resp struct {
		ConfigOptions      []acpOption `json:"configOptions"`
		ConfigOptionsSnake []acpOption `json:"config_options"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return acpEffortOption{}, false
	}
	options := resp.ConfigOptions
	if len(options) == 0 {
		options = resp.ConfigOptionsSnake
	}

	for _, opt := range options {
		id := strings.TrimSpace(opt.ID)
		if !acpEffortOptionIDs[strings.ToLower(id)] &&
			!acpEffortOptionIDs[strings.ToLower(strings.TrimSpace(opt.Category))] {
			continue
		}
		// An option we can read but not address is useless: without an id
		// there is nothing to send back to session/set_config_option.
		if id == "" {
			continue
		}
		result := acpEffortOption{ConfigID: id}
		seen := map[string]bool{}
		for _, choice := range opt.Options {
			value := strings.TrimSpace(choice.Value)
			if value == "" || seen[value] {
				continue
			}
			seen[value] = true
			label := strings.TrimSpace(choice.Name)
			if label == "" {
				label = strings.Title(value) //nolint:staticcheck
			}
			result.Choices = append(result.Choices, ThinkingLevel{Value: value, Label: label})
		}
		current := strings.TrimSpace(opt.CurrentValue)
		if current == "" {
			current = strings.TrimSpace(opt.CurrentValueSnake)
		}
		// Only echo a default we also advertise. A currentValue outside the
		// choice list (CodeBuddy's `enabled`) becomes an empty DefaultLevel,
		// which the UI renders as a generic "Default" instead of a level the
		// picker cannot offer.
		if result.supports(current) {
			result.CurrentValue = current
		}
		return result, true
	}
	return acpEffortOption{}, false
}

// annotateACPThinkingFromSession fills in each model's effort catalog from the
// selector carried by the SAME session/new response the models came from, so
// the catalog costs no extra process.
//
// Every model shares one catalog because ACP scopes configOptions to the
// session, not the model: a backend's effort selector is independent of which
// model the session is on. Per-model catalogs need a separate structured
// interface from the CLI — `codex debug models --bundled`, `kimi provider list`
// — which ACP does not offer. ModelThinking is still per model, so a runtime
// that later exposes per-model efforts can diverge without touching the UI.
//
// Models keep a nil Thinking when nothing is advertised: no picker beats a
// picker that does nothing.
func annotateACPThinkingFromSession(models []Model, sessionResult json.RawMessage) {
	option, ok := parseACPEffortOption(sessionResult)
	if !ok || len(option.Choices) == 0 {
		return
	}
	thinking := &ModelThinking{
		SupportedLevels: option.Choices,
		DefaultLevel:    option.CurrentValue,
	}
	for i := range models {
		models[i].Thinking = thinking
	}
}

// acpRequestFn is the ACP JSON-RPC call each backend's client already exposes.
// Taking it as a function keeps this helper independent of which client struct
// a given backend spawned.
type acpRequestFn func(ctx context.Context, method string, params any) (json.RawMessage, error)

// applyACPEffortOption pushes a persisted thinking level onto a live ACP
// session using the selector that session advertised.
//
// A configuration failure never blocks the task: the prompt goes out either
// way and the warnings are the only record that the level shown in the UI may
// not be the level in effect. Which level that is depends on how it failed —
// on a request error the session keeps what it had (the CLI's own setting on a
// fresh session, the previous turn's level on a resumed one); on an
// unconfirmed reply it sits at whatever the backend reports.
//
// The read-back is the load-bearing part. `set_config_option` answering
// success does not mean the agent applied anything: Kimi ≤0.28.1 confirms "on"
// after being set to "max", and Hermes records the value without wiring it
// into the agent. Re-reading currentValue is the only signal available at
// runtime that separates a runtime that honours the level from one that just
// accepts the call.
//
// resumed says whether sessionResult came from session/resume rather than
// session/new. It only changes how loudly a missing option is reported: ACP
// does not require resume to re-advertise configOptions, and a resumed session
// already carries the previous turn's level, so silence there is expected
// rather than a fault. The cost is that changing an agent's level mid-session
// takes effect on the next fresh session — see the reasonix call site.
func applyACPEffortOption(
	ctx context.Context,
	request acpRequestFn,
	backend string,
	logger *slog.Logger,
	sessionID string,
	sessionResult json.RawMessage,
	level string,
	resumed bool,
) {
	if level == "" {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}

	option, ok := parseACPEffortOption(sessionResult)
	if !ok || len(option.Choices) == 0 {
		if resumed {
			// Expected: the session keeps whatever level it was created with.
			// Warning here would fire on every turn of every resumed task.
			logger.Debug("resumed session re-advertises no reasoning-effort option; keeping the level it already has",
				"backend", backend,
				"requested_level", level,
			)
			return
		}
		// A fresh session that advertises nothing is a real gap: the agent
		// carries a level the server accepted against a previously discovered
		// catalog. A CLI downgrade, a different account, or a model without
		// the dial all land here.
		logger.Warn("session advertises no reasoning-effort option; sending the prompt without it",
			"backend", backend,
			"requested_level", level,
		)
		return
	}
	if !option.supports(level) {
		// Sending an unadvertised token invites a hard error from the backend
		// on a call whose failure we deliberately swallow. Skipping keeps the
		// session on a level it actually understands.
		logger.Warn("session does not advertise the requested reasoning effort; sending the prompt without it",
			"backend", backend,
			"config_id", option.ConfigID,
			"requested_level", level,
			"advertised_levels", strings.Join(option.values(), ","),
		)
		return
	}

	result, err := request(ctx, "session/set_config_option", map[string]any{
		"sessionId": sessionID,
		"configId":  option.ConfigID,
		"value":     level,
	})
	if err != nil {
		logger.Warn("runtime rejected the reasoning effort request; sending the prompt anyway",
			"backend", backend,
			"config_id", option.ConfigID,
			"requested_level", level,
			"effective_level", "unchanged",
			"error", err,
		)
		return
	}

	effectiveLevel, confirmed := acpConfigOptionCurrentValue(result, option.ConfigID)
	if !confirmed || effectiveLevel != level {
		if !confirmed {
			effectiveLevel = "unknown"
		}
		logger.Warn("runtime did not confirm the requested reasoning effort; sending the prompt anyway",
			"backend", backend,
			"config_id", option.ConfigID,
			"requested_level", level,
			"effective_level", effectiveLevel,
		)
		return
	}
	logger.Info("session reasoning effort confirmed",
		"backend", backend,
		"config_id", option.ConfigID,
		"level", effectiveLevel,
	)
}
