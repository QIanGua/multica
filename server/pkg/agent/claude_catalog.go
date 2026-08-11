package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// claude_catalog.go turns Claude Code's model catalog from a hand-maintained
// table into discovered data, where the host has the credentials to discover it.
//
// Every other runtime learns its catalog from its own CLI: `codex debug models
// --bundled` carries supported_reasoning_levels, `kimi provider list --json`
// carries supportEfforts. Claude Code has no such command — `claude --help`
// advertises `--model` and `--effort` but nothing that enumerates models — so
// claudeStaticModels and claudeModelEffortAllow were hand-edited on every
// Anthropic release, and a model released after the last Multica release had no
// picker at all (MUL-6020).
//
// The Anthropic Models API publishes exactly the two facts those tables hold:
//
//	GET /v1/models → { id, display_name, capabilities: { effort: {
//	    supported, low: {supported}, …, max: {supported} } } }
//
// It is an HTTP endpoint rather than a CLI subcommand, which is the whole
// difficulty: it needs an Anthropic credential, and a Claude Code user does not
// necessarily have one this process can read. Subscription and OAuth logins keep
// their tokens in Claude Code's own store, not in the environment.
//
// So discovery here is CREDENTIAL-GATED, and its absence is not a failure:
//
//   - Credential present  → the API is authoritative for per-model effort and
//     additive for the model list.
//   - Credential absent   → behaviour is byte-for-byte what it was before this
//     file existed. No request is made.
//
// Three rules keep the gate honest:
//
//   - The credential is only ever read from the environment this process was
//     given — never from a user config file or an OS keychain. Reading those
//     would turn a catalog refresh into credential exfiltration from a store the
//     user pointed at a different tool.
//   - The only caller is the daemon's model-list task (agent.ListModels is
//     reached exclusively through Daemon.listModels), so the credential and the
//     request are always the user's own machine's. The Multica server never
//     makes this call, and a shared server-side key can never end up describing
//     a user's picker.
//   - The levels the API reports are still intersected with what the local
//     `claude --help` advertises (see projectClaudeLevels). The API describes
//     what Anthropic's models accept; the installed CLI is what actually has to
//     take `--effort`. The narrower of the two is the only honest answer.

const (
	// claudeAPIDefaultBaseURL is Anthropic's public API host. ANTHROPIC_BASE_URL
	// overrides it, because a user who points Claude Code at a gateway or proxy
	// has a credential scoped to that host, not to api.anthropic.com.
	claudeAPIDefaultBaseURL = "https://api.anthropic.com"
	// claudeAPIVersion is the dated API contract this parser was written
	// against. It is required on every request.
	claudeAPIVersion = "2023-06-01"
	// claudeAPIOAuthBeta is required alongside a bearer token; API-key requests
	// must not send it.
	claudeAPIOAuthBeta = "oauth-2025-04-20"
	// claudeAPITimeout bounds the whole catalog fetch. Model discovery runs on a
	// daemon heartbeat action, so this is time the model-list task spends
	// waiting on someone else's network.
	claudeAPITimeout = 8 * time.Second
	// claudeAPICatalogTTL is how long one successful snapshot is reused. The
	// catalog changes when Anthropic ships a model, so hours is right; the point
	// of caching is to keep the picker off the network, not to be fresh.
	claudeAPICatalogTTL = 30 * time.Minute
	// claudeAPIMaxPages bounds pagination. The catalog is tens of models; this
	// exists so a server that always answers has_more cannot loop forever.
	claudeAPIMaxPages = 5
	// claudeAPIPageLimit is the page size requested from the endpoint.
	claudeAPIPageLimit = 100
)

// claudeAPIModel is the subset of a Models API entry this package consumes.
type claudeAPIModel struct {
	ID           string
	DisplayName  string
	EffortLevels map[string]bool
}

// claudeAPIEffortLevels is the level vocabulary read out of `capabilities.effort`.
// It mirrors claudeEffortLabel; a level Anthropic adds later is picked up when
// this list and the label map are extended together.
var claudeAPIEffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// claudeAPICredential resolves the credential and the header it belongs on.
//
// Resolution order matches the Anthropic SDKs and the `ant` CLI:
// ANTHROPIC_API_KEY, then ANTHROPIC_AUTH_TOKEN. The two are NOT
// interchangeable on the wire — an API key goes on `x-api-key`, an OAuth token
// on `Authorization: Bearer` plus the oauth beta header — so sending the wrong
// pairing is a 401, not a fallback. Profile files and OS keychains are
// deliberately not consulted: see the file comment.
func claudeAPICredential(env func(string) string) (header, value string, oauth bool, ok bool) {
	if key := strings.TrimSpace(env("ANTHROPIC_API_KEY")); key != "" {
		return "x-api-key", key, false, true
	}
	if token := strings.TrimSpace(env("ANTHROPIC_AUTH_TOKEN")); token != "" {
		return "Authorization", "Bearer " + token, true, true
	}
	return "", "", false, false
}

// claudeAPIBaseURL returns the host to query, honouring ANTHROPIC_BASE_URL.
func claudeAPIBaseURL(env func(string) string) string {
	if base := strings.TrimSpace(env("ANTHROPIC_BASE_URL")); base != "" {
		return strings.TrimSuffix(base, "/")
	}
	return claudeAPIDefaultBaseURL
}

// fetchClaudeAPIModels reads the whole model catalog from the Models API.
//
// Returns ok=false for every "we don't know" case — no credential, network
// failure, non-2xx, unparsable body — because all of them mean the same thing
// to the caller: keep the static catalog. Only a successful, non-empty parse
// counts as an answer.
func fetchClaudeAPIModels(ctx context.Context, env func(string) string, client *http.Client) ([]claudeAPIModel, bool) {
	header, value, oauth, ok := claudeAPICredential(env)
	if !ok {
		return nil, false
	}
	if client == nil {
		client = &http.Client{Timeout: claudeAPITimeout}
	}
	baseURL := claudeAPIBaseURL(env)

	reqCtx, cancel := context.WithTimeout(ctx, claudeAPITimeout)
	defer cancel()

	var out []claudeAPIModel
	afterID := ""
	for page := 0; page < claudeAPIMaxPages; page++ {
		url := fmt.Sprintf("%s/v1/models?limit=%d", baseURL, claudeAPIPageLimit)
		if afterID != "" {
			url += "&after_id=" + afterID
		}
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
		if err != nil {
			return nil, false
		}
		req.Header.Set(header, value)
		req.Header.Set("anthropic-version", claudeAPIVersion)
		if oauth {
			req.Header.Set("anthropic-beta", claudeAPIOAuthBeta)
		}

		resp, err := client.Do(req)
		if err != nil {
			// Never log err: a proxy URL can carry credentials in userinfo.
			slog.Debug("Anthropic Models API unreachable; keeping the static Claude catalog")
			return nil, false
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || readErr != nil {
			// Status only. A 401/403 body can echo back key material, and this
			// runs on every cache miss so it would repeat forever.
			slog.Debug("Anthropic Models API refused the catalog request; keeping the static Claude catalog",
				"status", resp.StatusCode,
			)
			return nil, false
		}

		models, hasMore, lastID, parseOK := parseClaudeAPIModelsPage(body)
		if !parseOK {
			slog.Debug("Anthropic Models API returned an unrecognised catalog shape; keeping the static Claude catalog")
			return nil, false
		}
		out = append(out, models...)
		if !hasMore || lastID == "" {
			break
		}
		afterID = lastID
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// parseClaudeAPIModelsPage decodes one `GET /v1/models` page.
//
// A model whose `capabilities.effort` block is absent gets a nil EffortLevels,
// which means "the API did not describe this model's effort" and leaves the
// hand-maintained table in charge for that model alone. That is deliberately
// different from `effort.supported: false`, which is a real answer — no levels —
// and produces an empty, non-nil map so the picker is hidden rather than
// inheriting the superset.
func parseClaudeAPIModelsPage(body []byte) (models []claudeAPIModel, hasMore bool, lastID string, ok bool) {
	type capabilityFlag struct {
		Supported bool `json:"supported"`
	}
	type wireModel struct {
		ID           string `json:"id"`
		DisplayName  string `json:"display_name"`
		Capabilities *struct {
			Effort *struct {
				Supported bool           `json:"supported"`
				Low       capabilityFlag `json:"low"`
				Medium    capabilityFlag `json:"medium"`
				High      capabilityFlag `json:"high"`
				XHigh     capabilityFlag `json:"xhigh"`
				Max       capabilityFlag `json:"max"`
			} `json:"effort"`
		} `json:"capabilities"`
	}
	var payload struct {
		Data    []wireModel `json:"data"`
		HasMore bool        `json:"has_more"`
		LastID  string      `json:"last_id"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false, "", false
	}
	if payload.Data == nil {
		// No `data` array at all: this is not a models page (an error envelope,
		// a proxy's HTML, a different endpoint). Distinguishable from a valid
		// empty page, which decodes to a non-nil empty slice.
		return nil, false, "", false
	}

	out := make([]claudeAPIModel, 0, len(payload.Data))
	for _, m := range payload.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		entry := claudeAPIModel{ID: id, DisplayName: strings.TrimSpace(m.DisplayName)}
		if m.Capabilities != nil && m.Capabilities.Effort != nil {
			effort := m.Capabilities.Effort
			levels := map[string]bool{}
			if effort.Supported {
				for level, supported := range map[string]bool{
					"low":    effort.Low.Supported,
					"medium": effort.Medium.Supported,
					"high":   effort.High.Supported,
					"xhigh":  effort.XHigh.Supported,
					"max":    effort.Max.Supported,
				} {
					if supported {
						levels[level] = true
					}
				}
			}
			if effort.Supported && len(levels) == 0 {
				// "Supported, but no level we recognise" is not an answer, it is
				// a shape we cannot read — a renamed or re-nested level block.
				// Taking it literally would mean "supported with zero levels",
				// which hides the picker; and since the shape is the same for
				// every entry in the catalog, one upstream rename would do that
				// to every Claude model at once, precisely on the hosts where
				// discovery works. Fall back to undescribed so the
				// hand-maintained table — still correct here — stays in charge.
				//
				// This is the one branch of the schema not validated against a
				// live response (no Anthropic credential on the machine this was
				// written on), so it fails toward the known-good answer.
				slog.Debug("Anthropic Models API reported effort support with no recognised level; using the static per-model table for this model",
					"model", id,
				)
				entry.EffortLevels = nil
			} else {
				// An explicit `supported: false` keeps its empty (non-nil) map:
				// that one is unambiguous, and hiding the picker is correct.
				entry.EffortLevels = levels
			}
		}
		out = append(out, entry)
	}
	return out, payload.HasMore, strings.TrimSpace(payload.LastID), true
}

// ── Snapshot cache ───────────────────────────────────────────────────

type claudeAPICatalogEntry struct {
	models    []claudeAPIModel
	ok        bool
	expiresAt time.Time
}

var (
	claudeAPICatalogMu    sync.Mutex
	claudeAPICatalogCache claudeAPICatalogEntry
)

// resetClaudeAPICatalogForTests is exposed for tests only; production code
// relies on the TTL or a process restart.
func resetClaudeAPICatalogForTests() {
	claudeAPICatalogMu.Lock()
	claudeAPICatalogCache = claudeAPICatalogEntry{}
	claudeAPICatalogMu.Unlock()
}

// claudeAPICatalogFetch is the fetch indirection tests replace. Production code
// must not reassign it.
var claudeAPICatalogFetch = func(ctx context.Context) ([]claudeAPIModel, bool) {
	return fetchClaudeAPIModels(ctx, os.Getenv, nil)
}

// claudeAPICatalog returns the cached Models API snapshot, fetching when stale.
//
// Failures are cached too, unlike the model-catalog caches elsewhere in this
// package. Those cache only successes because an empty catalog there is a
// transient CLI hiccup worth retrying immediately. Here the overwhelmingly
// common failure is structural — no credential on this host, ever — and
// retrying it on every discovery would put a doomed network call in front of a
// picker that already has a correct static answer.
func claudeAPICatalog(ctx context.Context) ([]claudeAPIModel, bool) {
	claudeAPICatalogMu.Lock()
	if time.Now().Before(claudeAPICatalogCache.expiresAt) {
		models, ok := claudeAPICatalogCache.models, claudeAPICatalogCache.ok
		claudeAPICatalogMu.Unlock()
		return models, ok
	}
	claudeAPICatalogMu.Unlock()

	models, ok := claudeAPICatalogFetch(ctx)

	claudeAPICatalogMu.Lock()
	claudeAPICatalogCache = claudeAPICatalogEntry{
		models:    models,
		ok:        ok,
		expiresAt: time.Now().Add(claudeAPICatalogTTL),
	}
	claudeAPICatalogMu.Unlock()
	return models, ok
}

// ── Merging ──────────────────────────────────────────────────────────

// mergeClaudeModels combines the discovered catalog with the static one.
//
// The merge is a UNION, not a replacement, and that asymmetry is the point.
// Codex and Kimi replace their static list on a successful discovery because
// discovery runs through the same binary and the same account that will execute
// the task — what it reports is what the CLI will accept. Here the two are
// different auth contexts: the credential in the environment belongs to an API
// organisation, while Claude Code may be running on a subscription login that
// can reach models that organisation cannot. Replacing would then delete
// working models from the picker to fix a staleness problem.
//
// So: every discovered model appears (this is what makes a new release show up
// without a Multica release), and every static model survives (this is what
// makes the change safe). Discovered order leads because it is the fresher
// signal; static-only entries keep their relative order after it.
func mergeClaudeModels(static []Model, discovered []claudeAPIModel, discoveredOK bool) []Model {
	if !discoveredOK || len(discovered) == 0 {
		return static
	}

	staticByID := make(map[string]Model, len(static))
	for _, m := range static {
		staticByID[m.ID] = m
	}
	// The static table owns which model is the everyday default; discovery has
	// no opinion about it. Carry the flag over only if that model still exists.
	defaultID := ""
	for _, m := range static {
		if m.Default {
			defaultID = m.ID
			break
		}
	}

	out := make([]Model, 0, len(discovered)+len(static))
	seen := make(map[string]bool, len(discovered)+len(static))
	for _, d := range discovered {
		entry := Model{ID: d.ID, Label: d.DisplayName, Provider: "anthropic"}
		if prior, ok := staticByID[d.ID]; ok && entry.Label == "" {
			// display_name is normally present; fall back to the curated label
			// rather than rendering a bare model id.
			entry.Label = prior.Label
		}
		if entry.Label == "" {
			entry.Label = d.ID
		}
		entry.Default = d.ID == defaultID
		out = append(out, entry)
		seen[d.ID] = true
	}
	for _, m := range static {
		if seen[m.ID] {
			continue
		}
		out = append(out, m)
		seen[m.ID] = true
	}
	return out
}

// claudeEffortAllowForModel decides which effort levels a model may offer,
// before the local CLI's own superset narrows them further.
//
// Discovery wins where it spoke, because `capabilities.effort` is the same fact
// claudeModelEffortAllow was hand-transcribing — from the publisher rather than
// from a release-note read. Where it stayed silent (no credential, or a model
// the API did not describe) the hand table answers exactly as before. A model
// neither source describes gets nil, which means "no per-model restriction" and
// leaves the CLI superset in charge — the pre-existing behaviour for an
// unrecognised model.
func claudeEffortAllowForModel(modelID string, discovered map[string]map[string]bool) map[string]bool {
	if levels, ok := discovered[modelID]; ok && levels != nil {
		return levels
	}
	return claudeModelEffortAllow[modelID]
}

// claudeDiscoveredEffortAllow indexes a snapshot by model id, keeping only the
// models the API actually described effort for.
func claudeDiscoveredEffortAllow(discovered []claudeAPIModel, ok bool) map[string]map[string]bool {
	if !ok {
		return nil
	}
	out := make(map[string]map[string]bool, len(discovered))
	for _, m := range discovered {
		if m.EffortLevels != nil {
			out[m.ID] = m.EffortLevels
		}
	}
	return out
}

// claudeCatalogFingerprint identifies one discovered snapshot so a cache keyed
// by it cannot serve a mapping built from a different catalog. Empty for "no
// discovery", which is the state every host without a credential stays in.
func claudeCatalogFingerprint(discovered []claudeAPIModel, ok bool) string {
	if !ok || len(discovered) == 0 {
		return ""
	}
	parts := make([]string, 0, len(discovered))
	for _, m := range discovered {
		levels := make([]string, 0, len(m.EffortLevels))
		for _, level := range claudeAPIEffortLevels {
			if m.EffortLevels[level] {
				levels = append(levels, level)
			}
		}
		described := "-"
		if m.EffortLevels != nil {
			described = strings.Join(levels, "+")
		}
		parts = append(parts, m.ID+":"+described)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}
