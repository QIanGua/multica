package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func envFrom(pairs map[string]string) func(string) string {
	return func(key string) string { return pairs[key] }
}

// ── Credential gate ──────────────────────────────────────────────────

// TestClaudeAPICredentialPairing: an API key and an OAuth token are not
// interchangeable on the wire, so the header (and the oauth beta) must follow
// the credential kind. Sending the wrong pairing is a 401, not a fallback.
func TestClaudeAPICredentialPairing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		env        map[string]string
		wantHeader string
		wantValue  string
		wantOAuth  bool
		wantOK     bool
	}{
		{
			name:       "api key",
			env:        map[string]string{"ANTHROPIC_API_KEY": "sk-ant-key"},
			wantHeader: "x-api-key",
			wantValue:  "sk-ant-key",
			wantOK:     true,
		},
		{
			name:       "auth token",
			env:        map[string]string{"ANTHROPIC_AUTH_TOKEN": "sk-ant-oat01-tok"},
			wantHeader: "Authorization",
			wantValue:  "Bearer sk-ant-oat01-tok",
			wantOAuth:  true,
			wantOK:     true,
		},
		{
			// Matches the SDK/CLI resolution order.
			name: "api key wins over auth token",
			env: map[string]string{
				"ANTHROPIC_API_KEY":    "sk-ant-key",
				"ANTHROPIC_AUTH_TOKEN": "sk-ant-oat01-tok",
			},
			wantHeader: "x-api-key",
			wantValue:  "sk-ant-key",
			wantOK:     true,
		},
		{name: "no credential", env: map[string]string{}},
		{name: "blank credential", env: map[string]string{"ANTHROPIC_API_KEY": "   "}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			header, value, oauth, ok := claudeAPICredential(envFrom(tc.env))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if header != tc.wantHeader || value != tc.wantValue || oauth != tc.wantOAuth {
				t.Fatalf("got (%q, %q, oauth=%v), want (%q, %q, oauth=%v)",
					header, value, oauth, tc.wantHeader, tc.wantValue, tc.wantOAuth)
			}
		})
	}
}

// TestFetchClaudeAPIModelsWithoutCredentialMakesNoRequest is the load-bearing
// privacy property: a host with no Anthropic credential must behave exactly as
// it did before this code existed, which means issuing no request at all.
func TestFetchClaudeAPIModelsWithoutCredentialMakesNoRequest(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("catalog fetch hit the network without a credential")
	}))
	defer server.Close()

	models, ok := fetchClaudeAPIModels(context.Background(),
		envFrom(map[string]string{"ANTHROPIC_BASE_URL": server.URL}), server.Client())
	if ok || models != nil {
		t.Fatalf("got (%v, %v), want (nil, false)", models, ok)
	}
}

// ── Fetch ────────────────────────────────────────────────────────────

func TestFetchClaudeAPIModelsSendsContract(t *testing.T) {
	t.Parallel()

	var gotAuth, gotVersion, gotBeta, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotVersion = r.Header.Get("anthropic-version")
		gotBeta = r.Header.Get("anthropic-beta")
		gotPath = r.URL.Path
		fmt.Fprint(w, `{"data":[{"id":"claude-opus-9","display_name":"Claude Opus 9"}],"has_more":false}`)
	}))
	defer server.Close()

	models, ok := fetchClaudeAPIModels(context.Background(), envFrom(map[string]string{
		"ANTHROPIC_AUTH_TOKEN": "tok",
		"ANTHROPIC_BASE_URL":   server.URL,
	}), server.Client())
	if !ok || len(models) != 1 || models[0].ID != "claude-opus-9" {
		t.Fatalf("got (%v, %v), want the single discovered model", models, ok)
	}
	if gotPath != "/v1/models" {
		t.Fatalf("path = %q, want /v1/models", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "Bearer tok")
	}
	if gotVersion != claudeAPIVersion {
		t.Fatalf("anthropic-version = %q, want %q", gotVersion, claudeAPIVersion)
	}
	// The oauth beta is required for a bearer token and must NOT ride along on
	// an API-key request.
	if gotBeta != claudeAPIOAuthBeta {
		t.Fatalf("anthropic-beta = %q, want %q", gotBeta, claudeAPIOAuthBeta)
	}
}

func TestFetchClaudeAPIModelsAPIKeyOmitsOAuthBeta(t *testing.T) {
	t.Parallel()

	var gotKey, gotBeta string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotBeta = r.Header.Get("anthropic-beta")
		fmt.Fprint(w, `{"data":[{"id":"claude-opus-9"}],"has_more":false}`)
	}))
	defer server.Close()

	if _, ok := fetchClaudeAPIModels(context.Background(), envFrom(map[string]string{
		"ANTHROPIC_API_KEY":  "sk-ant-key",
		"ANTHROPIC_BASE_URL": server.URL,
	}), server.Client()); !ok {
		t.Fatal("fetch failed for an api-key credential")
	}
	if gotKey != "sk-ant-key" {
		t.Fatalf("x-api-key = %q, want %q", gotKey, "sk-ant-key")
	}
	if gotBeta != "" {
		t.Fatalf("anthropic-beta = %q on an api-key request; want it absent", gotBeta)
	}
}

func TestFetchClaudeAPIModelsPaginates(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("after_id") {
		case "":
			fmt.Fprint(w, `{"data":[{"id":"a"}],"has_more":true,"last_id":"a"}`)
		case "a":
			fmt.Fprint(w, `{"data":[{"id":"b"}],"has_more":false}`)
		default:
			t.Errorf("unexpected after_id %q", r.URL.Query().Get("after_id"))
		}
	}))
	defer server.Close()

	models, ok := fetchClaudeAPIModels(context.Background(), envFrom(map[string]string{
		"ANTHROPIC_API_KEY":  "k",
		"ANTHROPIC_BASE_URL": server.URL,
	}), server.Client())
	if !ok || len(models) != 2 || models[0].ID != "a" || models[1].ID != "b" {
		t.Fatalf("got (%v, %v), want both pages", models, ok)
	}
}

// TestFetchClaudeAPIModelsPaginationIsBounded: a server that always answers
// has_more must not loop forever, and must not fail the caller either — the
// static catalog is still a correct answer.
func TestFetchClaudeAPIModelsPaginationIsBounded(t *testing.T) {
	t.Parallel()

	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		fmt.Fprintf(w, `{"data":[{"id":"m%d"}],"has_more":true,"last_id":"m%d"}`, pages, pages)
	}))
	defer server.Close()

	models, ok := fetchClaudeAPIModels(context.Background(), envFrom(map[string]string{
		"ANTHROPIC_API_KEY":  "k",
		"ANTHROPIC_BASE_URL": server.URL,
	}), server.Client())
	if !ok {
		t.Fatal("bounded pagination should still return what it read")
	}
	if pages != claudeAPIMaxPages || len(models) != claudeAPIMaxPages {
		t.Fatalf("read %d pages / %d models, want %d of each", pages, len(models), claudeAPIMaxPages)
	}
}

// TestFetchClaudeAPIModelsFailuresKeepStaticCatalog: every "we don't know" path
// must report ok=false so the caller keeps the static catalog. None of them may
// look like a successful empty catalog, which would blank the picker.
func TestFetchClaudeAPIModelsFailuresKeepStaticCatalog(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		body    string
		noServe bool
	}{
		{name: "unauthorized", status: 401, body: `{"error":{"type":"authentication_error"}}`},
		{name: "server error", status: 500, body: `{}`},
		{name: "html from a proxy", status: 200, body: `<html>login</html>`},
		{name: "error envelope with 200", status: 200, body: `{"error":{"type":"permission_error"}}`},
		{name: "valid but empty catalog", status: 200, body: `{"data":[],"has_more":false}`},
		{name: "unreachable host", noServe: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			baseURL, client := server.URL, server.Client()
			if tc.noServe {
				server.Close()
			} else {
				defer server.Close()
			}

			models, ok := fetchClaudeAPIModels(context.Background(), envFrom(map[string]string{
				"ANTHROPIC_API_KEY":  "k",
				"ANTHROPIC_BASE_URL": baseURL,
			}), client)
			if ok || models != nil {
				t.Fatalf("got (%v, %v), want (nil, false)", models, ok)
			}
		})
	}
}

// ── Parsing ──────────────────────────────────────────────────────────

// TestParseClaudeAPIModelsPageEffortCapabilities pins the three-way
// distinction the whole per-model story rests on: described-and-supported,
// described-as-unsupported, and not described at all.
func TestParseClaudeAPIModelsPageEffortCapabilities(t *testing.T) {
	t.Parallel()

	body := `{"data":[
		{"id":"opus","display_name":"Opus","capabilities":{"effort":{"supported":true,
			"low":{"supported":true},"medium":{"supported":true},"high":{"supported":true},
			"xhigh":{"supported":true},"max":{"supported":true}}}},
		{"id":"haiku","display_name":"Haiku","capabilities":{"effort":{"supported":true,
			"low":{"supported":true},"medium":{"supported":true},"high":{"supported":true},
			"xhigh":{"supported":false},"max":{"supported":false}}}},
		{"id":"legacy","display_name":"Legacy","capabilities":{"effort":{"supported":false}}},
		{"id":"undescribed","display_name":"Undescribed","capabilities":{}},
		{"id":"","display_name":"blank id is dropped"}
	],"has_more":false}`

	models, hasMore, _, ok := parseClaudeAPIModelsPage([]byte(body))
	if !ok || hasMore {
		t.Fatalf("parse ok=%v hasMore=%v, want (true, false)", ok, hasMore)
	}
	if len(models) != 4 {
		t.Fatalf("parsed %d models, want 4 (the blank id is dropped)", len(models))
	}

	byID := map[string]claudeAPIModel{}
	for _, m := range models {
		byID[m.ID] = m
	}
	if got := byID["opus"].EffortLevels; len(got) != 5 || !got["xhigh"] || !got["max"] {
		t.Fatalf("opus effort levels = %v, want all five", got)
	}
	if got := byID["haiku"].EffortLevels; len(got) != 3 || got["xhigh"] || got["max"] {
		t.Fatalf("haiku effort levels = %v, want low/medium/high only", got)
	}
	// `supported: false` is an answer, not a gap: an empty non-nil map means
	// "no levels", which hides the picker instead of inheriting the superset.
	if got := byID["legacy"].EffortLevels; got == nil || len(got) != 0 {
		t.Fatalf("legacy effort levels = %v, want an empty non-nil map", got)
	}
	// No effort block at all means the API said nothing, so the hand table must
	// stay in charge for that model.
	if got := byID["undescribed"].EffortLevels; got != nil {
		t.Fatalf("undescribed effort levels = %v, want nil", got)
	}
	if byID["opus"].DisplayName != "Opus" {
		t.Fatalf("display name = %q, want %q", byID["opus"].DisplayName, "Opus")
	}
}

// TestParseClaudeAPIModelsPageTreatsUnreadableEffortAsUndescribed guards the
// one shape this parser was never validated against a live response: an effort
// block that says `supported: true` but names no level we recognise.
//
// Read literally that is "supported, with zero levels", which projects to an
// empty allow-list and HIDES the picker — and because the shape would be the
// same for every model in the catalog, one upstream rename would silently take
// the thinking picker away from every Claude model at once, on exactly the
// hosts where discovery works. Since the hand-maintained table is still correct
// in that situation, unreadable must degrade to "not described" (nil) and leave
// it in charge, not to "no levels".
//
// `supported: false` stays a real answer: that one is unambiguous.
func TestParseClaudeAPIModelsPageTreatsUnreadableEffortAsUndescribed(t *testing.T) {
	t.Parallel()

	body := `{"data":[
		{"id":"drifted","capabilities":{"effort":{"supported":true,
			"levels":{"low":{"supported":true},"high":{"supported":true}}}}},
		{"id":"renamed","capabilities":{"effort":{"supported":true,
			"minimal":{"supported":true},"ultra":{"supported":true}}}},
		{"id":"explicitly-unsupported","capabilities":{"effort":{"supported":false}}}
	],"has_more":false}`

	models, _, _, ok := parseClaudeAPIModelsPage([]byte(body))
	if !ok {
		t.Fatal("a page with an unfamiliar effort shape is still a valid catalog page")
	}
	byID := map[string]claudeAPIModel{}
	for _, m := range models {
		byID[m.ID] = m
	}
	for _, id := range []string{"drifted", "renamed"} {
		if got := byID[id].EffortLevels; got != nil {
			t.Fatalf("%s: effort levels = %v, want nil so the static table stays in charge", id, got)
		}
		// The consequence that actually matters: the picker survives.
		allow := claudeEffortAllowForModel("claude-opus-5", map[string]map[string]bool{
			"claude-opus-5": byID[id].EffortLevels,
		})
		if len(projectClaudeLevels(claudeStaticEffortFullSuperset, allow)) == 0 {
			t.Fatalf("%s: unreadable effort shape wiped out the thinking picker", id)
		}
	}
	if got := byID["explicitly-unsupported"].EffortLevels; got == nil || len(got) != 0 {
		t.Fatalf("explicit supported:false = %v, want an empty non-nil map (a real 'no levels' answer)", got)
	}
}

func TestParseClaudeAPIModelsPageRejectsNonCatalog(t *testing.T) {
	t.Parallel()

	for _, body := range []string{
		`not json`,
		`{"error":{"type":"authentication_error"}}`,
		`{"has_more":false}`,
	} {
		if _, _, _, ok := parseClaudeAPIModelsPage([]byte(body)); ok {
			t.Fatalf("body %q parsed as a catalog page", body)
		}
	}
	// A genuinely empty page is well-formed; the fetch loop is what decides an
	// empty overall catalog is not an answer.
	if _, _, _, ok := parseClaudeAPIModelsPage([]byte(`{"data":[],"has_more":false}`)); !ok {
		t.Fatal("an empty data array is a valid page")
	}
}

// ── Redirect handling ────────────────────────────────────────────────

// TestFetchClaudeAPIModelsNeverFollowsRedirectWithCredential is a security
// regression test.
//
// net/http strips only Authorization / Www-Authenticate / Cookie / Cookie2 /
// Proxy-* when a redirect crosses domains — `x-api-key` is not on that list, so
// with the default redirect policy a 302 hands the user's Anthropic key to
// whatever host it names. ANTHROPIC_BASE_URL makes that reachable: it points at
// gateways and proxies we do not control.
func TestFetchClaudeAPIModelsNeverFollowsRedirectWithCredential(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		env     map[string]string
		leaked  func(*http.Request) string
		expects string
	}{
		{
			name:    "api key",
			env:     map[string]string{"ANTHROPIC_API_KEY": "sk-ant-secret"},
			leaked:  func(r *http.Request) string { return r.Header.Get("x-api-key") },
			expects: "sk-ant-secret",
		},
		{
			name:    "oauth token",
			env:     map[string]string{"ANTHROPIC_AUTH_TOKEN": "oat-secret"},
			leaked:  func(r *http.Request) string { return r.Header.Get("Authorization") },
			expects: "Bearer oat-secret",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var attackerSaw string
			var attackerHits int
			attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attackerHits++
				attackerSaw = tc.leaked(r)
				fmt.Fprint(w, `{"data":[{"id":"evil"}],"has_more":false}`)
			}))
			defer attacker.Close()

			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, attacker.URL+"/v1/models", http.StatusFound)
			}))
			defer origin.Close()

			env := map[string]string{"ANTHROPIC_BASE_URL": origin.URL}
			for k, v := range tc.env {
				env[k] = v
			}
			models, ok := fetchClaudeAPIModels(context.Background(), envFrom(env), origin.Client())

			if attackerHits != 0 {
				t.Fatalf("redirect target was contacted %d time(s); the credential request must not follow redirects", attackerHits)
			}
			if attackerSaw != "" {
				t.Fatalf("credential %q reached the redirect target", attackerSaw)
			}
			// An unfollowed 3xx is "no answer": keep the static catalog.
			if ok || models != nil {
				t.Fatalf("got (%v, %v) from a redirecting host, want (nil, false)", models, ok)
			}
		})
	}
}

// TestFetchClaudeAPIModelsDoesNotMutateCallerClient: the no-redirect policy is
// applied to a copy, so a client the caller owns keeps its own behaviour.
func TestFetchClaudeAPIModelsDoesNotMutateCallerClient(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"a"}],"has_more":false}`)
	}))
	defer server.Close()

	client := server.Client()
	if _, ok := fetchClaudeAPIModels(context.Background(), envFrom(map[string]string{
		"ANTHROPIC_API_KEY":  "k",
		"ANTHROPIC_BASE_URL": server.URL,
	}), client); !ok {
		t.Fatal("fetch failed")
	}
	if client.CheckRedirect != nil {
		t.Fatal("fetch installed its redirect policy on the caller's client")
	}
}

// ── Model list stays static ──────────────────────────────────────────

// TestListModelsClaudeNeverAddsDiscoveredModelIDs pins the decision taken after
// review of #6761: the model LIST is discovered per runtime while tasks execute
// per agent with the agent's own ANTHROPIC_API_KEY, so a discovered id is not
// evidence that the agent picking it can run it. Publishing one would be the
// "advertised but doesn't work" outcome this issue exists to prevent.
//
// Effort capabilities are not account-scoped and are still applied — that is
// the half this discovery is allowed to influence.
func TestListModelsClaudeNeverAddsDiscoveredModelIDs(t *testing.T) {
	// Mutates package-global caches and the fetch hook; must stay serial.
	resetClaudeAPICatalogForTests()
	resetThinkingCacheForTests()
	defer resetClaudeAPICatalogForTests()
	defer resetThinkingCacheForTests()

	original := claudeAPICatalogFetch
	claudeAPICatalogFetch = func(context.Context) ([]claudeAPIModel, bool) {
		return []claudeAPIModel{
			// A model only this credential's organisation can see.
			{ID: "claude-org-a-only", DisplayName: "Org A Only",
				EffortLevels: map[string]bool{"low": true}},
			// A model that IS in the static catalog, with narrower effort than
			// the hand table grants it.
			{ID: "claude-opus-5", DisplayName: "Claude Opus 5",
				EffortLevels: map[string]bool{"low": true, "medium": true, "high": true}},
		}, true
	}
	defer func() { claudeAPICatalogFetch = original }()

	catalog, err := ListModels(context.Background(), "claude", writeFakeClaudeHelpBinary(t))
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}

	static := claudeStaticModels()
	if len(catalog.Models) != len(static) {
		t.Fatalf("catalog has %d models, want the %d static ones — discovery must not add ids",
			len(catalog.Models), len(static))
	}
	byID := map[string]Model{}
	for i, m := range catalog.Models {
		byID[m.ID] = m
		if m.ID != static[i].ID {
			t.Fatalf("entry %d = %q, want %q", i, m.ID, static[i].ID)
		}
	}
	if _, leaked := byID["claude-org-a-only"]; leaked {
		t.Fatal("a discovered-only model reached the picker")
	}
	// The effort half still applies: the API narrowed opus-5 below the hand
	// table's five levels, and that is the answer we keep.
	if got := thinkingValues(byID["claude-opus-5"].Thinking); len(got) != 3 {
		t.Fatalf("claude-opus-5 levels = %v, want the API's three", got)
	}
}

// ── Effort allow-list resolution ─────────────────────────────────────

// TestClaudeEffortAllowForModelPrecedence: discovery answers where it spoke,
// the hand table answers where it did not, and an unknown model keeps the
// pre-existing "no per-model restriction" behaviour.
func TestClaudeEffortAllowForModelPrecedence(t *testing.T) {
	t.Parallel()

	discovered := map[string]map[string]bool{
		// Deliberately contradicts claudeModelEffortAllow, which grants
		// claude-opus-5 all five levels: the publisher wins over the transcript.
		"claude-opus-5": {"low": true, "medium": true, "high": true},
		"legacy-model":  {},
	}

	if got := claudeEffortAllowForModel("claude-opus-5", discovered); len(got) != 3 || got["xhigh"] {
		t.Fatalf("discovered model allow = %v, want the API's three levels", got)
	}
	// Not described by the API → hand table, exactly as before.
	want := claudeModelEffortAllow["claude-haiku-4-5-20251001"]
	if got := claudeEffortAllowForModel("claude-haiku-4-5-20251001", discovered); len(got) != len(want) {
		t.Fatalf("undescribed model allow = %v, want the static table %v", got, want)
	}
	// `effort.supported: false` → an empty non-nil map, which projects to no
	// levels and hides the picker rather than inheriting the CLI superset.
	got := claudeEffortAllowForModel("legacy-model", discovered)
	if got == nil || len(got) != 0 {
		t.Fatalf("unsupported-effort model allow = %v, want an empty non-nil map", got)
	}
	if levels := projectClaudeLevels(claudeStaticEffortFullSuperset, got); len(levels) != 0 {
		t.Fatalf("unsupported-effort model projected %v, want no levels", levels)
	}
	// Unknown to both → nil means "no restriction", the pre-existing behaviour.
	if got := claudeEffortAllowForModel("who-knows", discovered); got != nil {
		t.Fatalf("unknown model allow = %v, want nil", got)
	}
	if got := claudeEffortAllowForModel("claude-opus-5", nil); len(got) != 5 {
		t.Fatalf("with no discovery at all, allow = %v, want the static table's five levels", got)
	}
}

func TestClaudeDiscoveredEffortAllowSkipsUndescribedModels(t *testing.T) {
	t.Parallel()

	got := claudeDiscoveredEffortAllow([]claudeAPIModel{
		{ID: "described", EffortLevels: map[string]bool{"high": true}},
		{ID: "undescribed"},
	}, true)
	if len(got) != 1 || got["described"] == nil {
		t.Fatalf("index = %v, want only the described model", got)
	}
	if got := claudeDiscoveredEffortAllow([]claudeAPIModel{{ID: "x"}}, false); got != nil {
		t.Fatalf("index = %v on a failed discovery, want nil", got)
	}
}

// ── Fingerprint ──────────────────────────────────────────────────────

// TestClaudeCatalogFingerprintTracksCapabilities: the fingerprint is part of
// the thinking-cache key, so it must change whenever the mapping built from the
// snapshot would change — otherwise a newly discovered model shows no picker
// until the cache expires.
func TestClaudeCatalogFingerprintTracksCapabilities(t *testing.T) {
	t.Parallel()

	base := []claudeAPIModel{{ID: "a", EffortLevels: map[string]bool{"high": true}}}
	baseFP := claudeCatalogFingerprint(base, true)

	if fp := claudeCatalogFingerprint(nil, false); fp != "" {
		t.Fatalf("no-discovery fingerprint = %q, want empty", fp)
	}
	if fp := claudeCatalogFingerprint(base, true); fp != baseFP {
		t.Fatal("fingerprint is not stable for an identical snapshot")
	}
	// Order must not matter: the API may reorder its catalog between fetches.
	reordered := []claudeAPIModel{
		{ID: "b", EffortLevels: map[string]bool{"low": true}},
		{ID: "a", EffortLevels: map[string]bool{"high": true}},
	}
	forward := []claudeAPIModel{reordered[1], reordered[0]}
	if claudeCatalogFingerprint(reordered, true) != claudeCatalogFingerprint(forward, true) {
		t.Fatal("fingerprint changed on reorder alone")
	}
	for _, tc := range []struct {
		name     string
		snapshot []claudeAPIModel
	}{
		{name: "added model", snapshot: append(append([]claudeAPIModel{}, base...), claudeAPIModel{ID: "z"})},
		{name: "changed levels", snapshot: []claudeAPIModel{{ID: "a", EffortLevels: map[string]bool{"max": true}}}},
		{name: "levels removed", snapshot: []claudeAPIModel{{ID: "a", EffortLevels: map[string]bool{}}}},
		{name: "became undescribed", snapshot: []claudeAPIModel{{ID: "a"}}},
	} {
		if claudeCatalogFingerprint(tc.snapshot, true) == baseFP {
			t.Fatalf("%s: fingerprint unchanged", tc.name)
		}
	}
}

// ── End-to-end through ListModels ────────────────────────────────────

func TestListModelsClaudeWithoutCredentialIsStatic(t *testing.T) {
	// Mutates package-global caches and the fetch hook; must stay serial.
	resetClaudeAPICatalogForTests()
	resetThinkingCacheForTests()
	defer resetClaudeAPICatalogForTests()
	defer resetThinkingCacheForTests()

	original := claudeAPICatalogFetch
	claudeAPICatalogFetch = func(context.Context) ([]claudeAPIModel, bool) { return nil, false }
	defer func() { claudeAPICatalogFetch = original }()

	stub := writeFakeClaudeHelpBinary(t)
	catalog, err := ListModels(context.Background(), "claude", stub)
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}

	static := claudeStaticModels()
	if len(catalog.Models) != len(static) {
		t.Fatalf("got %d models, want the %d static ones", len(catalog.Models), len(static))
	}
	for i := range static {
		if catalog.Models[i].ID != static[i].ID || catalog.Models[i].Label != static[i].Label {
			t.Fatalf("entry %d = %+v, want %+v", i, catalog.Models[i], static[i])
		}
	}
	// The hand-maintained table still narrows per model, exactly as before.
	byID := map[string]Model{}
	for _, m := range catalog.Models {
		byID[m.ID] = m
	}
	if got := thinkingValues(byID["claude-haiku-4-5-20251001"].Thinking); len(got) != 3 {
		t.Fatalf("haiku levels = %v, want the static table's low/medium/high", got)
	}
}

func TestClaudeAPICatalogCachesFailures(t *testing.T) {
	// Mutates the package-global snapshot cache; must stay serial.
	resetClaudeAPICatalogForTests()
	defer resetClaudeAPICatalogForTests()

	calls := 0
	original := claudeAPICatalogFetch
	claudeAPICatalogFetch = func(context.Context) ([]claudeAPIModel, bool) {
		calls++
		return nil, false
	}
	defer func() { claudeAPICatalogFetch = original }()

	for i := 0; i < 3; i++ {
		if _, ok := claudeAPICatalog(context.Background()); ok {
			t.Fatal("claudeAPICatalog reported success for a failed fetch")
		}
	}
	// A host with no credential is the common case; retrying on every discovery
	// would put a doomed network call in front of a correct static answer.
	if calls != 1 {
		t.Fatalf("fetched %d times, want 1 (failures are cached too)", calls)
	}
}

func TestClaudeAPIBaseURL(t *testing.T) {
	t.Parallel()

	if got := claudeAPIBaseURL(envFrom(nil)); got != claudeAPIDefaultBaseURL {
		t.Fatalf("base url = %q, want %q", got, claudeAPIDefaultBaseURL)
	}
	// A user pointing Claude Code at a gateway has a credential scoped to that
	// gateway, not to api.anthropic.com.
	got := claudeAPIBaseURL(envFrom(map[string]string{"ANTHROPIC_BASE_URL": "https://gw.example.com/"}))
	if got != "https://gw.example.com" {
		t.Fatalf("base url = %q, want the trailing slash trimmed", got)
	}
}

// TestClaudeAPIEffortLevelsCoverLabelledLevels guards a quiet coupling: the
// fingerprint enumerates claudeAPIEffortLevels, so a level the parser reads but
// this list omits would change the mapping without changing the cache key.
// Anthropic adding a sixth level means extending both together.
func TestClaudeAPIEffortLevelsCoverLabelledLevels(t *testing.T) {
	t.Parallel()

	known := map[string]bool{}
	for _, level := range claudeAPIEffortLevels {
		known[level] = true
	}
	for level := range claudeEffortLabel {
		if !known[level] {
			t.Fatalf("claudeAPIEffortLevels is missing %q; the thinking-cache fingerprint would ignore it", level)
		}
	}
}
