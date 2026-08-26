package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// newLargeSkillFixture builds a skill whose file bodies dwarf its metadata,
// which is the condition GH #7498 reports: the response was large because it
// inlined every body, and the command that would have said so was the one that
// could not finish.
func newLargeSkillFixture(t *testing.T) (skillID string, bodies map[string]string) {
	t.Helper()

	skillID = dbfx.Insert(t, "skill", testutil.Cols{
		"workspace_id": testWorkspaceID,
		"name":         "skill-metadata-fixture",
		"description":  "fixture for include= tests",
		"content":      strings.Repeat("s", 4096),
		"created_by":   testUserID,
	})

	bodies = map[string]string{
		"reference.md":     strings.Repeat("r", 60_000),
		"scripts/setup.sh": strings.Repeat("x", 128),
	}
	for path, body := range bodies {
		dbfx.Insert(t, "skill_file", testutil.Cols{
			"skill_id": skillID,
			"path":     path,
			"content":  body,
		})
	}
	return skillID, bodies
}

func skillRequest(t *testing.T, skillID, path string) *http.Request {
	t.Helper()
	return withURLParam(newRequest(http.MethodGet, path, nil), "id", skillID)
}

func TestListSkillFilesOmitsBodiesByDefault(t *testing.T) {
	if testPool == nil {
		t.Skip("no database available")
	}
	skillID, bodies := newLargeSkillFixture(t)

	req := skillRequest(t, skillID, "/api/skills/"+skillID+"/files")
	var resp []SkillFileMetadataResponse
	res := testutil.Call(t, testHandler.ListSkillFiles, req).Want(http.StatusOK).JSON(&resp)

	if len(resp) != len(bodies) {
		t.Fatalf("got %d files, want %d", len(resp), len(bodies))
	}
	for _, f := range resp {
		want, ok := bodies[f.Path]
		if !ok {
			t.Fatalf("unexpected file %q in response", f.Path)
		}
		if f.Size != int64(len(want)) {
			t.Errorf("%s: size = %d, want %d", f.Path, f.Size, len(want))
		}
		if expect := contentHash(want); f.ContentHash != expect {
			t.Errorf("%s: content_hash = %q, want %q", f.Path, f.ContentHash, expect)
		}
	}

	// Asserting on the response bytes rather than on a missing struct field is
	// deliberate: a body that reappeared under a different key would satisfy a
	// field-name check and still reproduce the bug.
	if strings.Contains(res.Text(), strings.Repeat("r", 1000)) {
		t.Error("response inlined a file body")
	}
	if got, limit := res.Body.Len(), 4096; got > limit {
		t.Errorf("metadata listing is %d bytes, want <= %d", got, limit)
	}
}

func TestListSkillFilesIncludeContentReturnsBodies(t *testing.T) {
	if testPool == nil {
		t.Skip("no database available")
	}
	skillID, bodies := newLargeSkillFixture(t)

	req := skillRequest(t, skillID, "/api/skills/"+skillID+"/files?include=content")
	resp := testutil.Decode[[]SkillFileResponse](t, testHandler.ListSkillFiles, req, http.StatusOK)

	if len(resp) != len(bodies) {
		t.Fatalf("got %d files, want %d", len(resp), len(bodies))
	}
	for _, f := range resp {
		if want := bodies[f.Path]; f.Content != want {
			t.Errorf("%s: content length = %d, want %d", f.Path, len(f.Content), len(want))
		}
	}
}

// GetSkill keeps its current shape when asked for nothing in particular:
// installed web and desktop builds call it for the skill editor and cannot be
// retrofitted with a query parameter.
func TestGetSkillDefaultStillReturnsContent(t *testing.T) {
	if testPool == nil {
		t.Skip("no database available")
	}
	skillID, bodies := newLargeSkillFixture(t)

	req := skillRequest(t, skillID, "/api/skills/"+skillID)
	resp := testutil.Decode[SkillWithFilesResponse](t, testHandler.GetSkill, req, http.StatusOK)

	if len(resp.Content) != 4096 {
		t.Errorf("content length = %d, want 4096", len(resp.Content))
	}
	if len(resp.Files) != len(bodies) {
		t.Fatalf("got %d files, want %d", len(resp.Files), len(bodies))
	}
	for _, f := range resp.Files {
		if want := bodies[f.Path]; f.Content != want {
			t.Errorf("%s: content length = %d, want %d", f.Path, len(f.Content), len(want))
		}
	}
}

func TestGetSkillIncludeMetadataDropsBodies(t *testing.T) {
	if testPool == nil {
		t.Skip("no database available")
	}
	skillID, bodies := newLargeSkillFixture(t)

	req := skillRequest(t, skillID, "/api/skills/"+skillID+"?include=metadata")
	var resp SkillWithFileMetadataResponse
	res := testutil.Call(t, testHandler.GetSkill, req).Want(http.StatusOK).JSON(&resp)

	if resp.ID != skillID {
		t.Errorf("id = %q, want %q", resp.ID, skillID)
	}
	if resp.Name != "skill-metadata-fixture" {
		t.Errorf("name = %q, want the fixture name", resp.Name)
	}
	if resp.ContentSize != 4096 {
		t.Errorf("content_size = %d, want 4096", resp.ContentSize)
	}
	if want := contentHash(strings.Repeat("s", 4096)); resp.ContentHash != want {
		t.Errorf("content_hash = %q, want %q", resp.ContentHash, want)
	}
	if len(resp.Files) != len(bodies) {
		t.Fatalf("got %d files, want %d", len(resp.Files), len(bodies))
	}

	if strings.Contains(res.Text(), strings.Repeat("s", 1000)) {
		t.Error("response inlined the SKILL.md body")
	}
	if strings.Contains(res.Text(), strings.Repeat("r", 1000)) {
		t.Error("response inlined a file body")
	}
	if got, limit := res.Body.Len(), 4096; got > limit {
		t.Errorf("metadata response is %d bytes, want <= %d", got, limit)
	}
}

// A metadata response must stay small no matter how big the skill gets — that
// is the property the fix is actually about. Ten more files of 60KB each add
// rows, not megabytes.
func TestSkillMetadataSizeDoesNotTrackContentSize(t *testing.T) {
	if testPool == nil {
		t.Skip("no database available")
	}
	skillID, _ := newLargeSkillFixture(t)

	measure := func() int {
		req := skillRequest(t, skillID, "/api/skills/"+skillID+"/files")
		return testutil.Call(t, testHandler.ListSkillFiles, req).Want(http.StatusOK).Body.Len()
	}

	before := measure()
	for i := range 10 {
		dbfx.Insert(t, "skill_file", testutil.Cols{
			"skill_id": skillID,
			"path":     "bulk/" + string(rune('a'+i)) + ".md",
			"content":  strings.Repeat("b", 60_000),
		})
	}
	after := measure()

	// 600KB of new content; the listing may only grow by its 10 new rows.
	if grew := after - before; grew > 3000 {
		t.Errorf("listing grew by %d bytes after adding 600KB of content, want a per-row increase", grew)
	}
}

func TestSkillIncludeRejectsUnknownValue(t *testing.T) {
	if testPool == nil {
		t.Skip("no database available")
	}
	skillID, _ := newLargeSkillFixture(t)

	testutil.Call(t, testHandler.GetSkill,
		skillRequest(t, skillID, "/api/skills/"+skillID+"?include=everything")).
		Want(http.StatusBadRequest)

	testutil.Call(t, testHandler.ListSkillFiles,
		skillRequest(t, skillID, "/api/skills/"+skillID+"/files?include=everything")).
		Want(http.StatusBadRequest)
}
