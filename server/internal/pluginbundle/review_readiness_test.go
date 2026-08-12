package pluginbundle

import (
	"strings"
	"testing"
)

func TestReviewReadinessReleaseIsSignedAndValidated(t *testing.T) {
	release, files, err := ReviewReadinessRelease()
	if err != nil {
		t.Fatalf("ReviewReadinessRelease: %v", err)
	}
	if release.Manifest.Metadata.Key != ReviewReadinessPluginKey || release.Manifest.Metadata.Version != ReviewReadinessVersion {
		t.Fatalf("unexpected release identity: %s@%s", release.Manifest.Metadata.Key, release.Manifest.Metadata.Version)
	}
	if len(release.Signature) == 0 || len(files) != 2 {
		t.Fatalf("signature/files = %d/%d", len(release.Signature), len(files))
	}
	if !strings.Contains(string(files[1].Content), "# Review readiness") {
		t.Fatal("bundled review-readiness skill content missing")
	}
}
