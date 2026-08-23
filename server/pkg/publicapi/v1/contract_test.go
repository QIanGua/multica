package publicapiv1

import (
	"net/http"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestOpenAPICoversCapabilityLedger(t *testing.T) {
	var doc struct {
		OpenAPI string `yaml:"openapi"`
		Info    struct {
			Version string `yaml:"version"`
		} `yaml:"info"`
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(OpenAPI(), &doc); err != nil {
		t.Fatalf("parse embedded OpenAPI: %v", err)
	}
	if doc.OpenAPI != "3.1.0" || doc.Info.Version != "v1" {
		t.Fatalf("unexpected contract version: openapi=%q info.version=%q", doc.OpenAPI, doc.Info.Version)
	}

	want := make(map[string]map[string]bool)
	for _, operation := range Operations {
		methods := want[operation.Path]
		if methods == nil {
			methods = make(map[string]bool)
			want[operation.Path] = methods
		}
		methods[strings.ToLower(operation.Method)] = true
	}
	if len(doc.Paths) != len(want) {
		t.Fatalf("OpenAPI path count = %d, ledger path count = %d", len(doc.Paths), len(want))
	}
	for path, methods := range want {
		pathItem, ok := doc.Paths[path]
		if !ok {
			t.Errorf("OpenAPI is missing %s", path)
			continue
		}
		for method := range methods {
			if _, ok := pathItem[method]; !ok {
				t.Errorf("OpenAPI is missing %s %s", strings.ToUpper(method), path)
			}
		}
	}
	if _, exposed := doc.Paths["/hooks/{hook_key}"]; exposed {
		t.Fatal("Plugin-only person hook leaked into the public contract")
	}
}

func TestOperationLedgerPinsSharedScopes(t *testing.T) {
	want := map[string]string{
		http.MethodGet + " " + PathIssue:          "issues:read",
		http.MethodPatch + " " + PathIssue:        "issues:write",
		http.MethodGet + " " + PathIssueComments:  "comments:read",
		http.MethodPost + " " + PathIssueComments: "comments:write",
	}
	for _, operation := range Operations {
		if operation.Contract != ContractSharedResource {
			continue
		}
		key := operation.Method + " " + operation.Path
		if operation.Policy.Scope != want[key] {
			t.Errorf("%s scope = %q, want %q", key, operation.Policy.Scope, want[key])
		}
		if len(operation.Policy.Credentials) != 4 {
			t.Errorf("%s does not declare both user and Plugin credential families: %v", key, operation.Policy.Credentials)
		}
		if !operation.Policy.Audit {
			t.Errorf("%s must be audit-visible", key)
		}
		delete(want, key)
	}
	for key := range want {
		t.Errorf("shared operation missing from ledger: %s", key)
	}
}

func TestPluginExtensionsRejectUserCredentialsByContract(t *testing.T) {
	for _, operation := range Operations {
		if operation.Contract != ContractPluginExtension {
			continue
		}
		for _, credential := range operation.Policy.Credentials {
			if credential == CredentialUserOAuth || credential == CredentialPersonalAccess {
				t.Errorf("%s %s exposes Plugin extension to %s", operation.Method, operation.Path, credential)
			}
		}
	}
}
