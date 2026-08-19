package handler

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

// A task:message frame reaches every client in the workspace, so an unbounded
// tool input is a workspace-wide broadcast of that input. These tests pin the
// clipping applied to the realtime copy — and, just as importantly, that the
// persisted row it was built from is never touched (MUL-6396).

func TestTruncateTaskMessageForBroadcast_LeavesSmallPayloadsAlone(t *testing.T) {
	p := protocol.TaskMessagePayload{
		TaskID: "t1",
		Type:   "tool_use",
		Tool:   "Edit",
		Input:  map[string]any{"file_path": "/a/b.ts", "old_string": "x", "new_string": "y"},
		Output: "ok",
	}

	got := truncateTaskMessageForBroadcast(p)

	if got.Truncated {
		t.Fatalf("small payload marked truncated")
	}
	if got.Output != "ok" {
		t.Fatalf("output changed: %q", got.Output)
	}
	if len(got.Input) != 3 || got.Input["file_path"] != "/a/b.ts" {
		t.Fatalf("input changed: %#v", got.Input)
	}
}

func TestTruncateTaskMessageForBroadcast_ClipsOversizedOutput(t *testing.T) {
	p := protocol.TaskMessagePayload{Output: strings.Repeat("a", broadcastOutputLimit*2)}

	got := truncateTaskMessageForBroadcast(p)

	if !got.Truncated {
		t.Fatalf("oversized output not marked truncated")
	}
	if len(got.Output) > broadcastOutputLimit {
		t.Fatalf("output %d bytes, want <= %d", len(got.Output), broadcastOutputLimit)
	}
}

func TestTruncateTaskMessageForBroadcast_ClipsLargeStringsKeepsSmallOnes(t *testing.T) {
	// The shape of a Write of a large file: the path must survive so the
	// transcript can still say what was written.
	p := protocol.TaskMessagePayload{
		Input: map[string]any{
			"file_path": "/repo/big.ts",
			"content":   strings.Repeat("z", broadcastStringLimit*2),
		},
	}

	got := truncateTaskMessageForBroadcast(p)

	if !got.Truncated {
		t.Fatalf("oversized input not marked truncated")
	}
	if got.Input["file_path"] != "/repo/big.ts" {
		t.Fatalf("small field was clipped: %#v", got.Input["file_path"])
	}
	content, _ := got.Input["content"].(string)
	if len(content) > broadcastStringLimit {
		t.Fatalf("content %d bytes, want <= %d", len(content), broadcastStringLimit)
	}
}

func TestTruncateTaskMessageForBroadcast_ClipsNestedStrings(t *testing.T) {
	// MultiEdit: the large strings sit inside an array of objects.
	p := protocol.TaskMessagePayload{
		Input: map[string]any{
			"file_path": "/repo/x.ts",
			"edits": []any{
				map[string]any{"old_string": strings.Repeat("q", broadcastStringLimit*2), "new_string": "n"},
			},
		},
	}

	got := truncateTaskMessageForBroadcast(p)

	if !got.Truncated {
		t.Fatalf("nested oversized input not marked truncated")
	}
	edits, ok := got.Input["edits"].([]any)
	if !ok || len(edits) != 1 {
		t.Fatalf("edits lost: %#v", got.Input["edits"])
	}
	edit, _ := edits[0].(map[string]any)
	old, _ := edit["old_string"].(string)
	if len(old) > broadcastStringLimit {
		t.Fatalf("nested string %d bytes, want <= %d", len(old), broadcastStringLimit)
	}
	if edit["new_string"] != "n" {
		t.Fatalf("small nested field was clipped: %#v", edit["new_string"])
	}
}

func TestTruncateTaskMessageForBroadcast_DropsInputThatIsStillTooLargeInAggregate(t *testing.T) {
	// Per-string clipping bounds each value, not the total: a map of many
	// modest strings can still blow the budget.
	input := map[string]any{}
	for i := 0; i < 400; i++ {
		input[string(rune('a'+i%26))+strings.Repeat("k", 8)+string(rune(i))] = strings.Repeat("v", 200)
	}
	p := protocol.TaskMessagePayload{Input: input}

	got := truncateTaskMessageForBroadcast(p)

	if !got.Truncated {
		t.Fatalf("aggregate-oversized input not marked truncated")
	}
	if got.Input != nil {
		encoded, _ := json.Marshal(got.Input)
		t.Fatalf("input kept at %d bytes, want dropped", len(encoded))
	}
}

func TestTruncateTaskMessageForBroadcast_DoesNotSplitRunes(t *testing.T) {
	// A clip at a raw byte offset would leave a partial rune, which JSON
	// encoding then rewrites to U+FFFD.
	p := protocol.TaskMessagePayload{Output: strings.Repeat("世", broadcastOutputLimit)}

	got := truncateTaskMessageForBroadcast(p)

	if !utf8.ValidString(got.Output) {
		t.Fatalf("clipped output is not valid UTF-8")
	}
	if len(got.Output) > broadcastOutputLimit {
		t.Fatalf("output %d bytes, want <= %d", len(got.Output), broadcastOutputLimit)
	}
}

func TestTruncateTaskMessageForBroadcast_LeavesTheSourcePayloadIntact(t *testing.T) {
	// The same payload value feeds the REST list responses, which must keep
	// serving the full persisted content.
	big := strings.Repeat("z", broadcastStringLimit*2)
	edits := []any{map[string]any{"old_string": big}}
	input := map[string]any{"content": big, "edits": edits}
	p := protocol.TaskMessagePayload{Input: input, Output: strings.Repeat("a", broadcastOutputLimit*2)}

	_ = truncateTaskMessageForBroadcast(p)

	if got, _ := input["content"].(string); len(got) != len(big) {
		t.Fatalf("source input map was mutated: content is now %d bytes", len(got))
	}
	edit, _ := edits[0].(map[string]any)
	if got, _ := edit["old_string"].(string); len(got) != len(big) {
		t.Fatalf("source nested map was mutated: old_string is now %d bytes", len(got))
	}
	if len(p.Output) != broadcastOutputLimit*2 {
		t.Fatalf("source payload output was mutated")
	}
}
