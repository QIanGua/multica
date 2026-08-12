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
//   - Credential present  → the API may NARROW a model's hand-maintained effort
//     catalog (never widen it — see claudeEffortAllowForModel).
//   - Credential absent   → behaviour is byte-for-byte what it was before this
//     file existed. No request is made.
//
// Both of the hand-maintained tables therefore survive this file: the model list
// stays static outright ("Why the model LIST stays static" below), and the
// per-model effort table stays the baseline that discovery can only shrink.
// Neither can be replaced from here, because a runtime-scoped answer cannot
// speak for the per-agent credential and base URL a task actually executes
// with. Deleting them needs agent-scoped discovery, which is a protocol change.
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
	// Never follow a redirect on a request that carries a credential.
	//
	// net/http strips only Authorization / Www-Authenticate / Cookie / Cookie2 /
	// Proxy-* when a redirect crosses domains — `X-Api-Key` is not on that list,
	// so a 302 from the configured host would hand the user's Anthropic key to
	// whatever origin it names. ANTHROPIC_BASE_URL makes that reachable in
	// practice: it points at gateways and proxies we do not control. Even the
	// headers Go does strip are kept across a same-host HTTPS→HTTP downgrade.
	//
	// ErrUseLastResponse hands the 3xx back unfollowed, which the status check
	// below already treats as "no answer" → keep the static catalog. Copy the
	// client rather than mutating one the caller owns; the shallow copy shares
	// the transport, which is what we want.
	noRedirect := *client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	client = &noRedirect
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
	claudeAPICatalogMu sync.Mutex
	// claudeAPICatalogFetchMu serialises the fetch itself. Without it, every
	// concurrent cache miss issues its own request to an external API, and
	// whichever finishes LAST wins the cache — so a failed request could
	// overwrite a fresh successful snapshot. Held across the network call, with
	// a second cache check underneath, so the losers of the race read the
	// winner's result instead of repeating it.
	claudeAPICatalogFetchMu sync.Mutex
	claudeAPICatalogCache   claudeAPICatalogEntry
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
	if models, ok, fresh := claudeAPICatalogCached(); fresh {
		return models, ok
	}

	// One fetch at a time. Re-check underneath: a caller that queued behind the
	// winner must read its result rather than issue a second request.
	claudeAPICatalogFetchMu.Lock()
	defer claudeAPICatalogFetchMu.Unlock()
	if models, ok, fresh := claudeAPICatalogCached(); fresh {
		return models, ok
	}

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

func claudeAPICatalogCached() (models []claudeAPIModel, ok, fresh bool) {
	claudeAPICatalogMu.Lock()
	defer claudeAPICatalogMu.Unlock()
	if time.Now().Before(claudeAPICatalogCache.expiresAt) {
		return claudeAPICatalogCache.models, claudeAPICatalogCache.ok, true
	}
	return nil, false, false
}

// ── Why the model LIST stays static ──────────────────────────────────
//
// An earlier cut of this file merged the discovered model ids into the picker,
// so a model Anthropic shipped after the last Multica release would appear on
// its own. That is removed, and the reason is a credential-scope mismatch this
// layer cannot resolve (review of #6761):
//
//   - The catalog is discovered per RUNTIME. handleModelList is given a runtime
//     and an executable path, nothing else, and the answer is cached per runtime.
//   - Tasks execute per AGENT, and an agent's own ANTHROPIC_API_KEY is layered
//     into the child environment at launch (layerCustomEnvAndHermesHome —
//     ANTHROPIC_API_KEY is named in its doc comment as a typical value).
//
// So the key that answers "which models exist" is not necessarily the key that
// runs the task. A daemon-env key for organisation A would publish A's models
// into a picker used by an agent that executes with organisation B's key, or on
// a subscription login. The union direction protected against *removing* a model
// B can run; it did nothing about *adding* one only A can — which is the
// "advertised but doesn't work" outcome this whole issue exists to avoid, and
// the model list is exactly where we have no per-agent signal to fix it.
//
// `capabilities.effort` is reached by the same argument, one layer down: an
// agent can override ANTHROPIC_BASE_URL as well as the key, so the deployment
// behind a model id at execution time need not be the one that answered. It is
// kept only because it is confined to a direction that cannot go wrong — see
// claudeEffortAllowForModel, which lets discovery shrink the shipped level set
// and never widen it.
//
// Restoring dynamic model ids needs an agent-scoped catalog (the discovery
// request would have to carry the agent's resolved credentials) — a protocol
// change, tracked separately, not something a comment can make safe.

// claudeEffortAllowForModel decides which effort levels a model may offer,
// before the local CLI's own superset narrows them further.
//
// Discovery may only NARROW the hand-maintained baseline. It never adds a level
// claudeModelEffortAllow did not already grant, and it is ignored entirely for a
// model with no baseline.
//
// An earlier cut let discovery win outright, on the theory that which levels a
// model accepts is a property of the model rather than of the account asking.
// That is true of Anthropic's first-party API and false in general: an agent can
// set its own ANTHROPIC_BASE_URL ("router/proxy mode" is a documented use of the
// per-agent custom env, alongside ANTHROPIC_API_KEY), so the deployment behind a
// model id at execution time need not be the one the runtime-scoped catalog
// asked. Gateway A answering `xhigh` for a model that gateway B does not serve
// at that level would have published a level the task then silently ran without
// — the same "advertised but doesn't work" failure that removed the dynamic
// model list, one layer down.
//
// Intersecting is what makes the direction safe rather than the source
// trustworthy: whatever the discovery credential turns out to describe, the
// result is a subset of what this build already shipped, so no answer from the
// wrong context can widen the picker. What it still buys is the fail-closed
// direction — a level Anthropic stops supporting on an existing model
// disappears without waiting for a Multica release.
//
// A model with no baseline is skipped rather than narrowed: nil is the sentinel
// for "no curated per-model restriction", not a known-unrestricted set, so there
// is nothing here to intersect and narrowing it would be inventing a limit out
// of a context that may not be the executing one.
func claudeEffortAllowForModel(modelID string, discovered map[string]map[string]bool) map[string]bool {
	baseline := claudeModelEffortAllow[modelID]
	if baseline == nil {
		return nil
	}
	levels, ok := discovered[modelID]
	if !ok || levels == nil {
		return baseline
	}
	narrowed := make(map[string]bool, len(baseline))
	for level := range baseline {
		if levels[level] {
			narrowed[level] = true
		}
	}
	return narrowed
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
