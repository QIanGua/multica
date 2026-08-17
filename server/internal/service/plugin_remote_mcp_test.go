package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/plugincontract"
	"github.com/multica-ai/multica/server/pkg/pluginruntime"
)

func TestValidateRemoteMCPPublicConfigRejectsNestedSecretsAndSchemaViolations(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"required":["region"],
		"properties":{"region":{"type":"string","enum":["us","eu"]}},
		"additionalProperties":false
	}`)
	canonical, err := validateRemoteMCPPublicConfig(json.RawMessage(`{"region":"eu"}`), schema)
	if err != nil || string(canonical) != `{"region":"eu"}` {
		t.Fatalf("valid config = %s, %v", canonical, err)
	}
	for _, raw := range []string{
		`{"region":"ap"}`,
		`{"region":"eu","extra":true}`,
		`{"region":"eu","nested":{"access_token":"secret"}}`,
		`{"region":"eu","items":[{"password":"secret"}]}`,
	} {
		if _, err := validateRemoteMCPPublicConfig(json.RawMessage(raw), schema); err == nil {
			t.Fatalf("config %s was accepted", raw)
		}
	}
}

func TestSelectApprovedRemoteMCPToolsFailsClosed(t *testing.T) {
	declaration := plugincontract.RemoteMCPContribution{ToolIntent: []plugincontract.RemoteMCPToolIntent{
		{Name: "read", Risk: "read"}, {Name: "write", Risk: "write"},
	}}
	discovered := []pluginruntime.RemoteMCPTool{{Name: "read"}, {Name: "undeclared"}}
	approved, err := selectApprovedRemoteMCPTools(discovered, declaration, []string{"read"})
	if err != nil || len(approved) != 1 || approved[0].Name != "read" {
		t.Fatalf("approved = %#v, %v", approved, err)
	}
	for _, names := range [][]string{nil, {"undeclared"}, {"write"}, {"read", "read"}} {
		if _, err := selectApprovedRemoteMCPTools(discovered, declaration, names); err == nil {
			t.Fatalf("approval %v was accepted", names)
		}
	}
}

func TestSelectApprovedRemoteMCPToolsAllowsExplicitlyReviewedWildcard(t *testing.T) {
	declaration := plugincontract.RemoteMCPContribution{ToolIntent: []plugincontract.RemoteMCPToolIntent{
		{Name: plugincontract.RemoteMCPAnyToolIntent, Risk: "write"},
	}}
	discovered := []pluginruntime.RemoteMCPTool{{Name: "search-screens"}, {Name: "get-flow"}}
	applyDeclaredRisk(discovered, declaration)
	approved, err := selectApprovedRemoteMCPTools(discovered, declaration, []string{"search-screens"})
	if err != nil || len(approved) != 1 || approved[0].Name != "search-screens" || approved[0].Risk != "write" {
		t.Fatalf("approved = %#v, %v", approved, err)
	}
	if _, err := selectApprovedRemoteMCPTools(discovered, declaration, []string{"not-discovered"}); err == nil {
		t.Fatal("undiscovered wildcard tool was approved")
	}
}

func TestRemoteMCPCompilationRowReady(t *testing.T) {
	ready := db.ListPluginCompilationContributionsRow{
		ConfigID:       pgtype.UUID{Bytes: [16]byte{1}, Valid: true},
		ConfigRevision: pgtype.Int8{Int64: 1, Valid: true},
		Endpoint:       pgtype.Text{String: "https://mcp.example.com/mcp", Valid: true},
		AuthType:       pgtype.Text{String: "none", Valid: true},
		FailurePolicy:  pgtype.Text{String: "required", Valid: true},
		SchemaDigest:   pgtype.Text{String: "sha256:abc", Valid: true},
		ReviewedAt:     pgtype.Timestamptz{Time: time.Now(), Valid: true},
		ApprovedTools:  []byte(`[{"name":"fixture.read","input_schema":{"type":"object"},"schema_digest":"","risk":"read"}]`),
	}
	if !remoteMCPCompilationRowReady(ready) {
		t.Fatal("reviewed auth=none configuration reported not ready")
	}

	withSecret := ready
	withSecret.AuthType = pgtype.Text{String: "bearer", Valid: true}
	if remoteMCPCompilationRowReady(withSecret) {
		t.Fatal("bearer configuration without credential reported ready")
	}
	withSecret.SecretRef = pgtype.UUID{Bytes: [16]byte{2}, Valid: true}
	if !remoteMCPCompilationRowReady(withSecret) {
		t.Fatal("bearer configuration with credential reported not ready")
	}

	unconfigured := db.ListPluginCompilationContributionsRow{}
	if remoteMCPCompilationRowReady(unconfigured) {
		t.Fatal("never-configured contribution reported ready")
	}

	unreviewed := ready
	unreviewed.ReviewedAt = pgtype.Timestamptz{}
	if remoteMCPCompilationRowReady(unreviewed) {
		t.Fatal("unreviewed configuration reported ready")
	}

	noTools := ready
	noTools.ApprovedTools = []byte(`[]`)
	if remoteMCPCompilationRowReady(noTools) {
		t.Fatal("configuration without approved tools reported ready")
	}
}

func TestSecretHintDoesNotExposeShortCredential(t *testing.T) {
	if got := secretHint("abc"); got != "••••" || strings.Contains(got, "abc") {
		t.Fatalf("secretHint = %q", got)
	}
	if got := secretHint("credential"); got != "••••tial" {
		t.Fatalf("secretHint = %q", got)
	}
}

func TestRemoteMCPAuthHeaderRejectsProtocolAndHopByHopHeaders(t *testing.T) {
	if !validRemoteMCPAuthHeader("X-Example-Key") {
		t.Fatal("safe custom header was rejected")
	}
	for _, header := range []string{"Host", "Connection", "Content-Length", "Mcp-Session-Id", "Proxy-Authorization", "bad header"} {
		if validRemoteMCPAuthHeader(header) {
			t.Fatalf("unsafe header %q was accepted", header)
		}
	}
}

func TestSanitizeRemoteMCPReturnTo(t *testing.T) {
	if got := sanitizeRemoteMCPReturnTo("/workspace/settings?tab=plugins"); got != "/workspace/settings?tab=plugins" {
		t.Fatalf("safe return path changed to %q", got)
	}
	for _, unsafe := range []string{"https://evil.example", "//evil.example/path", "/ok\r\nLocation: https://evil.example"} {
		if got := sanitizeRemoteMCPReturnTo(unsafe); got != "/" {
			t.Errorf("unsafe return path %q became %q", unsafe, got)
		}
	}
}
