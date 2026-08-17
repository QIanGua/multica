package agent

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// prefixIndex reports where the first token of want starts inside argv, or -1.
func prefixIndex(argv, want []string) int {
	for i := 0; i+len(want) <= len(argv); i++ {
		if strings.Join(argv[i:i+len(want)], "\x00") == strings.Join(want, "\x00") {
			return i
		}
	}
	return -1
}

// TestLaunchPrefixPrecedesProtocolFlags is GH #7046 in one assertion. `ccms
// start q36` only reaches the real Claude binary after its `start` subcommand
// is selected, so a `-p` that arrives first is parsed by the wrapper, which
// rejects it outright ("unknown option '-p'"). The subcommand tokens have to
// come first.
func TestLaunchPrefixPrecedesProtocolFlags(t *testing.T) {
	t.Parallel()

	cfg := Config{ExecutablePath: "ccms", LaunchPrefix: []string{"start", "q36"}, Logger: slog.Default()}
	argv := cfg.commandAt("ccms").Argv(buildClaudeArgs(ExecOptions{}, slog.Default())...)

	subcommand := prefixIndex(argv, []string{"start", "q36"})
	if subcommand != 0 {
		t.Fatalf("fixed_args must be the argv prefix, found at %d: %v", subcommand, argv)
	}
	protocol := prefixIndex(argv, []string{"-p"})
	if protocol < 0 {
		t.Fatalf("protocol flags missing from argv: %v", argv)
	}
	if protocol < subcommand+2 {
		t.Fatalf("-p at %d must come after the `start q36` prefix: %v", protocol, argv)
	}
}

// TestLaunchPrefixKeepsFlagStyleWrappersWorking is the compatibility half of
// the same change. A flag-style prefix parses identically before or after the
// protocol flags, which is why prefix-first can be the single order rather
// than a per-runtime setting.
func TestLaunchPrefixKeepsFlagStyleWrappersWorking(t *testing.T) {
	t.Parallel()

	cfg := Config{ExecutablePath: "agent", LaunchPrefix: []string{"--model", "composer-2.5"}, Logger: slog.Default()}
	argv := cfg.commandAt("agent").Argv(buildClaudeArgs(ExecOptions{}, slog.Default())...)

	if idx := prefixIndex(argv, []string{"--model", "composer-2.5"}); idx != 0 {
		t.Fatalf("flag-style fixed_args must still lead the argv, found at %d: %v", idx, argv)
	}
}

// TestLaunchPrefixLosesToExplicitAgentModel pins the precedence flip this
// change makes deliberately. Before GH #7046 the prefix sat last and won every
// last-wins parse, so a runtime pinned to `--model composer-2.5` silently
// overrode the model the member picked in the UI. Prefix-first inverts that:
// the more specific per-agent choice is the later token and wins.
func TestLaunchPrefixLosesToExplicitAgentModel(t *testing.T) {
	t.Parallel()

	cfg := Config{ExecutablePath: "agent", LaunchPrefix: []string{"--model", "composer-2.5"}, Logger: slog.Default()}
	argv := cfg.commandAt("agent").Argv(buildClaudeArgs(ExecOptions{Model: "claude-opus-4-7"}, slog.Default())...)

	runtimeModel := prefixIndex(argv, []string{"--model", "composer-2.5"})
	agentModel := prefixIndex(argv, []string{"--model", "claude-opus-4-7"})
	if runtimeModel < 0 || agentModel < 0 {
		t.Fatalf("expected both --model occurrences: %v", argv)
	}
	if agentModel <= runtimeModel {
		t.Fatalf("the agent's explicit model must be the later --model (runtime at %d, agent at %d): %v",
			runtimeModel, agentModel, argv)
	}
}

// TestFilterLaunchPrefixKeepsSubcommandsDropsProtocolFlags covers the
// blocklist split: positional tokens are the command's identity and pass
// through, flag tokens compete with the daemon's own protocol arguments and do
// not. A blocked flag takes its value token with it.
func TestFilterLaunchPrefixKeepsSubcommandsDropsProtocolFlags(t *testing.T) {
	t.Parallel()

	got := filterLaunchPrefix(
		[]string{"start", "q36", "-p", "--output-format", "text", "--model", "composer-2.5"},
		"claude", slog.Default())
	want := []string{"start", "q36", "--model", "composer-2.5"}

	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("filterLaunchPrefix = %v, want %v", got, want)
	}
}

// TestNewFiltersLaunchPrefixOnce proves the filter runs at the single point
// that knows the protocol family, so no backend has to remember to call it.
func TestNewFiltersLaunchPrefixOnce(t *testing.T) {
	t.Parallel()

	backend, err := New("claude", Config{
		ExecutablePath: "ccms",
		LaunchPrefix:   []string{"start", "q36", "--output-format", "text"},
		Logger:         slog.Default(),
	})
	if err != nil {
		t.Fatalf("New(claude): %v", err)
	}
	got := backend.(*claudeBackend).cfg.LaunchPrefix
	want := []string{"start", "q36"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("New did not filter the launch prefix: got %v, want %v", got, want)
	}
}

// TestLaunchPrefixReachesACPFamilies is the regression guard for the half of
// the bug the report did not name: fixed_args used to ride on ExtraArgs, which
// the ACP backends never read, so on those families it was silently dropped
// rather than merely misordered. The prefix must land ahead of the hardcoded
// `acp` subcommand.
func TestLaunchPrefixReachesACPFamilies(t *testing.T) {
	t.Parallel()

	for _, family := range []string{"kimi", "hermes", "kiro", "reasonix", "qwenpaw"} {
		t.Run(family, func(t *testing.T) {
			t.Parallel()
			cfg := Config{LaunchPrefix: []string{"start", "q36"}, Logger: slog.Default()}
			argv := cfg.commandAt("wrapper").Argv("acp")
			if idx := prefixIndex(argv, []string{"start", "q36", "acp"}); idx != 0 {
				t.Fatalf("%s: prefix must precede the acp subcommand, got %v", family, argv)
			}
		})
	}
}

// TestDiscoveryCacheKeySeparatesLaunchPrefixes: one binary behind two
// different prefixes is two different catalogs.
func TestDiscoveryCacheKeySeparatesLaunchPrefixes(t *testing.T) {
	t.Parallel()

	q36 := discoveryCacheKey("claude", Command{Path: "/usr/local/bin/ccms", Prefix: []string{"start", "q36"}})
	opus := discoveryCacheKey("claude", Command{Path: "/usr/local/bin/ccms", Prefix: []string{"start", "opus"}})
	bare := discoveryCacheKey("claude", Command{Path: "/usr/local/bin/ccms"})

	if q36 == opus {
		t.Fatalf("two prefixes on one binary share a cache key: %q", q36)
	}
	if q36 == bare || opus == bare {
		t.Fatalf("a prefixed command must not share the bare binary's key: %q / %q / %q", q36, opus, bare)
	}
}

// TestCommandArgvNeverAliasesItsInputs: Argv results get appended to by
// callers, so a shared backing array would let one invocation's arguments
// leak into the next.
func TestCommandArgvNeverAliasesItsInputs(t *testing.T) {
	t.Parallel()

	prefix := []string{"start", "q36"}
	cmd := Command{Path: "ccms", Prefix: prefix}

	first := cmd.Argv("-p")
	first = append(first, "mutated")
	second := cmd.Argv("--version")

	if strings.Join(cmd.Prefix, "\x00") != "start\x00q36" {
		t.Fatalf("Argv mutated the command's prefix: %v", cmd.Prefix)
	}
	if prefixIndex(second, []string{"mutated"}) >= 0 {
		t.Fatalf("a later Argv saw an earlier call's append: %v", second)
	}
}

// TestOnlyLaunchGoSpawnsRuntimeProcesses is the structural half of this fix.
//
// Distributed opt-in is what let ExtraArgs rot: it was honoured by six of
// twenty-one backends, and MULTICA_QWENPAW_ARGS shipped plumbed-but-dropped
// because nothing failed when a backend forgot to read it. Re-establishing the
// same convention for the launch prefix would rot the same way, so the rule is
// mechanical instead: every runtime process in this package is constructed in
// launch.go, which is the one place that applies the prefix.
//
// A new backend that calls os/exec directly fails here rather than silently
// reintroducing GH #7046.
func TestOnlyLaunchGoSpawnsRuntimeProcesses(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	fset := token.NewFileSet()
	var offenders []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// launch.go owns the boundary. The platform invocation rewrites resolve
		// a PowerShell host with exec.LookPath but never spawn the runtime.
		if name == "launch.go" {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "exec" {
				return true
			}
			if sel.Sel.Name != "Command" && sel.Sel.Name != "CommandContext" {
				return true
			}
			offenders = append(offenders,
				fset.Position(call.Pos()).String()+": exec."+sel.Sel.Name)
			return true
		})
	}

	if len(offenders) > 0 {
		t.Fatalf("runtime processes must be built through Command.exec / Command.execVia in launch.go, "+
			"otherwise a custom runtime's fixed_args are dropped (GH #7046). Offending sites:\n%s",
			strings.Join(offenders, "\n"))
	}
}

// TestClaudeBackendLaunchesWrapperSubcommandFirst is the end-to-end proof:
// a real subprocess, spawned through the real backend, records the argv it was
// handed. The fake wrapper stands in for `ccms` and never touches a
// user-installed CLI.
func TestClaudeBackendLaunchesWrapperSubcommandFirst(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	argsFile := filepath.Join(dir, "argv")
	wrapper := filepath.Join(dir, "ccms")
	writeTestExecutable(t, wrapper, []byte("#!/bin/sh\n"+
		"for arg in \"$@\"; do printf '%s\\n' \"$arg\" >> \""+argsFile+"\"; done\n"+
		"exit 0\n"))

	backend, err := New("claude", Config{
		ExecutablePath: wrapper,
		LaunchPrefix:   []string{"start", "q36"},
		Logger:         slog.Default(),
	})
	if err != nil {
		t.Fatalf("New(claude): %v", err)
	}
	session, err := backend.Execute(context.Background(), "hello", ExecOptions{Cwd: dir})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	go func() {
		for range session.Messages {
		}
	}()
	<-session.Result

	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("wrapper recorded no argv: %v", err)
	}
	argv := strings.Fields(string(bytes.TrimSpace(raw)))
	if idx := prefixIndex(argv, []string{"start", "q36"}); idx != 0 {
		t.Fatalf("wrapper was launched as %v; `start q36` must lead", argv)
	}
	if idx := prefixIndex(argv, []string{"-p"}); idx < 2 {
		t.Fatalf("-p reached the wrapper before its subcommand: %v", argv)
	}
}

// TestLaunchPrefixOutranksExtraArgs locks the full argv contract that replaced
// "fixed_args are prepended to ExtraArgs" (#4408). The old arrangement put the
// prefix inside ExtraArgs, which lands *after* the protocol flags — and which
// fifteen of the twenty-one backends never read at all. The prefix now has its
// own slot ahead of everything.
func TestLaunchPrefixOutranksExtraArgs(t *testing.T) {
	t.Parallel()

	cfg := Config{LaunchPrefix: []string{"start", "q36"}, Logger: slog.Default()}
	argv := cfg.commandAt("ccms").Argv(buildClaudeArgs(ExecOptions{
		ExtraArgs:  []string{"--extra-arg"},
		CustomArgs: []string{"--custom-arg"},
	}, slog.Default())...)

	prefix := prefixIndex(argv, []string{"start", "q36"})
	protocol := prefixIndex(argv, []string{"-p"})
	extra := prefixIndex(argv, []string{"--extra-arg"})
	custom := prefixIndex(argv, []string{"--custom-arg"})

	if prefix != 0 {
		t.Fatalf("launch prefix must lead: %v", argv)
	}
	if !(prefix < protocol && protocol < extra && extra < custom) {
		t.Fatalf("want prefix < protocol < extra < custom, got %d/%d/%d/%d: %v",
			prefix, protocol, extra, custom, argv)
	}
}

// TestBuiltinRuntimeIdentitiesFilterLaunchPrefix: NewRuntime composes New, so
// a runtime identity (omp) inherits the same prefix policy as its family.
func TestBuiltinRuntimeIdentitiesFilterLaunchPrefix(t *testing.T) {
	t.Parallel()

	backend, err := ResolveBackend("omp", Config{
		ExecutablePath: "wrapper",
		LaunchPrefix:   []string{"start", "q36", "-p"},
		Logger:         slog.Default(),
	})
	if err != nil {
		t.Fatalf("ResolveBackend(omp): %v", err)
	}
	got := backend.(*piBackend).cfg.LaunchPrefix
	if strings.Join(got, "\x00") != "start\x00q36" {
		t.Fatalf("runtime identity did not inherit prefix filtering: %v", got)
	}
}

// TestCodexLaunchPrefixCannotShadowManagedConfig: prefix-first wins ordinary
// flag conflicts on purpose, but Codex `-c key=value` beats the daemon-written
// config.toml from any argv position. A fixed_args entry aimed at a managed
// namespace has to be removed, not merely outranked.
func TestCodexLaunchPrefixCannotShadowManagedConfig(t *testing.T) {
	t.Parallel()

	cmd := Command{Prefix: []string{
		"proxy", "run",
		"-c", "mcp_servers.evil.command=/bin/sh",
		"-c", "shell_environment_policy.inherit=all",
		"--model", "gpt-5",
	}}

	filtered := cmd.
		withFilteredPrefix(func(p []string) []string { return filterCodexShellEnvConfigOverrides(p, slog.Default()) }).
		withFilteredPrefix(func(p []string) []string { return filterCodexCustomConfigOverrides(p, slog.Default()) })

	want := []string{"proxy", "run", "--model", "gpt-5"}
	if strings.Join(filtered.Prefix, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("prefix = %v, want the managed-namespace overrides dropped and %v kept",
			filtered.Prefix, want)
	}
}
