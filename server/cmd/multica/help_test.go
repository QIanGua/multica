package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRootHelpDocumentsMulticaCLI pins the MUL-6662 guidance to the surface that
// actually renders it. `multica --help` is served by rootHelpTemplate, which
// hardcodes its prose — rootCmd.Long is never printed — so documenting the
// variable on the command struct would have shipped text no user can see.
//
// The daemon exports MULTICA_CLI with the absolute path of the binary it runs
// (applySelfBinaryEnv); a bare `multica` resolves through PATH, which is what
// let an unrelated older install answer a daemon-local call (GH #7520). An agent
// that reaches --help instead of the runtime brief has to find the rule here.
func TestRootHelpDocumentsMulticaCLI(t *testing.T) {
	out := renderHelp(t, "")

	if !strings.Contains(out, "MULTICA_CLI") {
		t.Fatal("`multica --help` does not mention MULTICA_CLI; the guidance is invisible to anyone reading help instead of the runtime brief")
	}
	if !strings.Contains(out, `"$MULTICA_CLI" <command>`) {
		t.Error("root help does not show the quoted POSIX invocation; the path can contain spaces")
	}
	// `$MULTICA_CLI` is a PowerShell variable, not the environment. A Windows
	// reader copying only the POSIX form runs an empty command name.
	if !strings.Contains(out, "$env:MULTICA_CLI <command>") {
		t.Error("root help does not show the PowerShell invocation form")
	}
}

// TestRepoCheckoutHelpDocumentsMulticaCLI covers the leaf that broke. Unlike the
// root, leafHelpTemplate does render .Long, so this asserts the command whose
// old help ("Used by agents to check out repos on demand") taught the bare name.
func TestRepoCheckoutHelpDocumentsMulticaCLI(t *testing.T) {
	out := renderHelp(t, "repo", "checkout")

	if !strings.Contains(out, "MULTICA_CLI") {
		t.Fatal("`repo checkout --help` does not mention MULTICA_CLI")
	}
	if !strings.Contains(out, "PowerShell") {
		t.Error("`repo checkout --help` shows only the POSIX form; the command runs on Windows too")
	}
}

// renderHelp captures the help output for a command path, with the same
// templates main() installs.
func renderHelp(t *testing.T, path ...string) string {
	t.Helper()

	initHelp(rootCmd)
	cmd := rootCmd
	if len(path) > 0 && path[0] != "" {
		found, _, err := rootCmd.Find(path)
		if err != nil {
			t.Fatalf("find %v: %v", path, err)
		}
		cmd = found
	}

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	t.Cleanup(func() { cmd.SetOut(nil) })
	if err := cmd.Help(); err != nil {
		t.Fatalf("render help for %v: %v", path, err)
	}
	return buf.String()
}
