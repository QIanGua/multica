package dbid

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// The tables whose primary keys the application mints as UUIDv7, and the sqlc
// query that inserts each one. Keeping the list here — rather than deriving it —
// is deliberate: adding a table to the v7 set is a decision, and it should show
// up as an edit to this list in review.
//
// query is the sqlc query name; arg is the parameter name the query binds the id
// to (CreateRetryTask already had an `id` parameter for the PARENT task, so its
// new id is named new_task_id); field is the Go field on the params struct.
var v7Writes = []struct {
	table string
	file  string
	query string
	arg   string
	field string
}{
	{"activity_log", "activity.sql", "CreateActivity", "id", "ID"},
	{"agent_task_queue", "agent.sql", "CreateAgentTask", "id", "ID"},
	{"agent_task_queue", "agent.sql", "CreateDeferredChannelIssueTask", "id", "ID"},
	{"agent_task_queue", "agent.sql", "CreateQuickCreateTask", "id", "ID"},
	{"agent_task_queue", "agent.sql", "CreateDeferredAgentTask", "id", "ID"},
	{"agent_task_queue", "agent.sql", "CreateRetryTask", "new_task_id", "NewTaskID"},
	{"agent_task_queue", "autopilot.sql", "CreateAutopilotTask", "id", "ID"},
	{"agent_task_queue", "chat.sql", "CreateChatTask", "id", "ID"},
	{"autopilot_run", "autopilot.sql", "CreateAutopilotRun", "id", "ID"},
	{"channel_inbound_audit", "channel.sql", "RecordChannelInboundDrop", "id", "ID"},
	{"chat_message", "chat.sql", "CreateChatMessage", "id", "ID"},
	{"chat_message", "chat.sql", "CreateMikaOnboardingOpening", "id", "ID"},
	{"chat_session", "chat.sql", "CreateChatSession", "id", "ID"},
	{"comment", "comment.sql", "CreateComment", "id", "ID"},
	{"inbox_item", "inbox.sql", "CreateInboxItem", "id", "ID"},
	{"issue", "issue.sql", "CreateIssue", "id", "ID"},
	{"issue", "issue.sql", "CreateIssueWithOrigin", "id", "ID"},
	{"task_token", "task_token.sql", "CreateTaskToken", "id", "ID"},
	{"webhook_delivery", "webhook_delivery.sql", "CreateWebhookDelivery", "id", "ID"},
}

var queryNameRE = regexp.MustCompile(`(?m)^--\s*name:\s*(\w+)\s*:(\w+)`)

// TestV7QueriesBindAnIDWithADatabaseFallback locks in the shape of every
// converted INSERT: it must accept an id from the application AND keep
// gen_random_uuid() as the fallback, so a caller that leaves the parameter unset
// degrades to the pre-change behaviour instead of violating NOT NULL.
func TestV7QueriesBindAnIDWithADatabaseFallback(t *testing.T) {
	for _, w := range v7Writes {
		t.Run(w.query, func(t *testing.T) {
			block := queryBlock(t, w.file, w.query)

			want := fmt.Sprintf("COALESCE(sqlc.narg('%s')::uuid, gen_random_uuid())", w.arg)
			if !strings.Contains(block, want) {
				t.Errorf("%s in %s does not bind its id as %s\n%s", w.query, w.file, want, block)
			}

			insert := fmt.Sprintf("INSERT INTO %s (", w.table)
			i := strings.Index(block, insert)
			if i < 0 {
				t.Fatalf("%s does not INSERT INTO %s", w.query, w.table)
			}
			cols := block[i+len(insert):]
			cols = cols[:strings.Index(cols, ")")]
			if !hasColumn(cols, "id") {
				t.Errorf("%s does not list the id column:\ncolumns: %s", w.query, strings.Join(strings.Fields(cols), " "))
			}
		})
	}
}

// TestEveryV7CallSiteMintsAnID walks the production Go sources and fails if any
// literal of a converted params struct leaves the id unset. Without this a new
// call site added later would silently go back to a random v4 id — the
// COALESCE fallback makes that failure quiet, so it needs a test to stay loud.
func TestEveryV7CallSiteMintsAnID(t *testing.T) {
	fields := map[string]*regexp.Regexp{}
	for _, w := range v7Writes {
		fields[w.query+"Params"] = regexp.MustCompile(`\b` + w.field + `:\s+dbid\.NewV7\(\)`)
	}

	found := map[string]int{}
	for _, path := range productionGoFiles(t) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(src)
		for typ, mint := range fields {
			needle := "." + typ + "{"
			for i := 0; ; {
				j := strings.Index(text[i:], needle)
				if j < 0 {
					break
				}
				at := i + j
				lit := structLiteral(text[at+len(needle):])
				found[typ]++
				if !mint.MatchString(lit) {
					t.Errorf("%s:%d: %s literal does not mint an id (%s)",
						path, 1+strings.Count(text[:at], "\n"), typ, mint)
				}
				i = at + len(needle)
			}
		}
	}

	for typ := range fields {
		if found[typ] == 0 {
			t.Errorf("no production call site found for %s — was the query removed or renamed?", typ)
		}
	}
}

// queryBlock returns the text of one named sqlc query, from its `-- name:`
// comment to the start of the next one.
func queryBlock(t *testing.T, file, query string) string {
	t.Helper()

	path := filepath.Join(serverDir(t), "pkg", "db", "queries", file)
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	marks := queryNameRE.FindAllStringSubmatchIndex(text, -1)
	for i, m := range marks {
		if text[m[2]:m[3]] != query {
			continue
		}
		end := len(text)
		if i+1 < len(marks) {
			end = marks[i+1][0]
		}
		return text[m[0]:end]
	}
	t.Fatalf("query %s not found in %s", query, file)
	return ""
}

// structLiteral returns the body of a composite literal whose opening brace has
// already been consumed, tracking nesting so an inner literal does not end it.
func structLiteral(after string) string {
	depth := 1
	for i, r := range after {
		switch r {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return after[:i]
			}
		}
	}
	return after
}

func productionGoFiles(t *testing.T) []string {
	t.Helper()

	var out []string
	root := serverDir(t)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// generated/ holds the sqlc output, which never constructs its own
			// params structs, and testdata/ is not compiled.
			if d.Name() == "generated" || d.Name() == "testdata" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatalf("no production Go files found under %s", root)
	}
	return out
}

func serverDir(t *testing.T) string {
	t.Helper()

	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve this test's path")
	}
	// .../server/pkg/dbid/writes_test.go -> .../server
	return filepath.Dir(filepath.Dir(filepath.Dir(self)))
}

func hasColumn(list, name string) bool {
	for _, part := range strings.Split(list, ",") {
		if strings.TrimSpace(part) == name {
			return true
		}
	}
	return false
}
