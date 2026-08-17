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

// TestLaunchPrefixPlacesFlagStyleWrappersFirst pins where a flag-style prefix
// lands. It asserts the argv Multica builds, not that any particular third-party
// parser accepts the new position — most treat the two orders as equivalent,
// but a CLI that separates global from subcommand flags may not, which is why
// the docs call the move out instead of promising it is invisible.
func TestLaunchPrefixPlacesFlagStyleWrappersFirst(t *testing.T) {
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

// TestFilterLaunchPrefixKeepsPositionalCollidingWithSubcommand is Elon's
// must-fix 2. The blocklists are shared with custom_args, and several carry
// positional tokens — hermes/qwenpaw block `acp`, traecli blocks `acp` and
// `serve`, grok blocks `agent`, `stdio` and `serve`. Those entries exist to
// stop someone re-issuing the backend's own subcommand through custom_args.
// A launch prefix is the opposite case: the token is the command's identity,
// and dropping it rewrites the operator's command into a different one.
func TestFilterLaunchPrefixKeepsPositionalCollidingWithSubcommand(t *testing.T) {
	t.Parallel()

	cases := []struct {
		family string
		prefix []string
		want   []string
	}{
		{"hermes", []string{"acp", "tenant"}, []string{"acp", "tenant"}},
		{"qwenpaw", []string{"acp", "tenant"}, []string{"acp", "tenant"}},
		{"traecli", []string{"serve", "acp"}, []string{"serve", "acp"}},
		{"grok", []string{"agent", "stdio"}, []string{"agent", "stdio"}},
		{"grok", []string{"leader", "headless"}, []string{"leader", "headless"}},
		// Flags in the same prefix are still filtered; grok blocks -p.
		{"grok", []string{"agent", "-p", "stdio"}, []string{"agent", "stdio"}},
	}
	for _, tc := range cases {
		t.Run(tc.family+"/"+strings.Join(tc.prefix, "_"), func(t *testing.T) {
			t.Parallel()
			got := filterLaunchPrefix(tc.prefix, tc.family, slog.Default())
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Fatalf("filterLaunchPrefix(%v, %s) = %v, want %v", tc.prefix, tc.family, got, tc.want)
			}
		})
	}
}

// TestFilterLaunchPrefixConsumesBlockedFlagValue: dropping a blocked flag must
// take its value with it, or the leftover value token would be read as a
// subcommand — precisely the kind of token this filter otherwise preserves.
func TestFilterLaunchPrefixConsumesBlockedFlagValue(t *testing.T) {
	t.Parallel()

	got := filterLaunchPrefix(
		[]string{"start", "--permission-mode", "ask", "q36", "--output-format=text"},
		"claude", slog.Default())
	want := []string{"start", "q36"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("filterLaunchPrefix = %v, want %v", got, want)
	}
}

// TestStripAllHermesProfileArgs is Elon's must-fix 1, at the parser level.
// Stripping only the selection hermes acts on today promotes the next one to
// first, and the task walks straight out of the overlay the daemon built.
func TestStripAllHermesProfileArgs(t *testing.T) {
	t.Parallel()

	got := StripAllHermesProfileArgs([]string{
		"-p", "research", "acp", "--profile=ops", "--model", "x", "-p", "third",
	})
	want := []string{"acp", "--model", "x"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("StripAllHermesProfileArgs = %v, want %v", got, want)
	}
	if sel := ParseHermesProfileArgs(got); sel.Found {
		t.Fatalf("a profile selection survived stripping: %+v", sel)
	}
}

// TestHermesLaunchPrefixCannotEscapeOverlay ties must-fix 1 to the argv that
// actually reaches hermes: prefix then custom args, with `acp` appended by the
// backend. Nothing in the final command line may re-point HERMES_HOME.
func TestHermesLaunchPrefixCannotEscapeOverlay(t *testing.T) {
	t.Parallel()

	prefix := StripAllHermesProfileArgs([]string{"hermes-wrapper-sub", "-p", "research"})
	custom := StripAllHermesProfileArgs([]string{"--model", "x", "--profile=ops"})

	cfg := Config{LaunchPrefix: prefix, Logger: slog.Default()}
	argv := cfg.commandAt("wrapper").Argv(append([]string{"acp"}, custom...)...)

	if sel := ParseHermesProfileArgs(argv); sel.Found {
		t.Fatalf("final hermes argv still selects a profile (%+v): %v", sel, argv)
	}
	// The non-profile tokens survive: stripping must not eat the command.
	if prefixIndex(argv, []string{"hermes-wrapper-sub", "acp"}) != 0 {
		t.Fatalf("stripping damaged the command identity: %v", argv)
	}
	if prefixIndex(argv, []string{"--model", "x"}) < 0 {
		t.Fatalf("stripping dropped an unrelated custom arg: %v", argv)
	}
}

// TestCodexLaunchPrefixCannotOverrideExplicitFastMode is Elon's must-fix 3.
// `--disable fast_mode` beats `--enable fast_mode` regardless of argv order and
// `-c features.fast_mode=false` beats config.toml from any position, so being
// first does not make the prefix lose. Both spellings have to be removed.
func TestCodexLaunchPrefixCannotOverrideExplicitFastMode(t *testing.T) {
	t.Parallel()

	for _, prefix := range [][]string{
		{"proxy", "run", "--disable", "fast_mode"},
		{"proxy", "run", "--disable=fast_mode"},
		{"proxy", "run", "-c", "features.fast_mode=false"},
	} {
		t.Run(strings.Join(prefix, "_"), func(t *testing.T) {
			t.Parallel()
			got := stripCodexFastModeConflicts(prefix, slog.Default())
			want := []string{"proxy", "run"}
			if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
				t.Fatalf("stripCodexFastModeConflicts(%v) = %v, want %v", prefix, got, want)
			}
			// The managed enable is appended once, by buildCodexArgs, not here.
			if prefixIndex(got, []string{"--enable"}) >= 0 {
				t.Fatalf("the strip helper must not append the managed enable: %v", got)
			}
		})
	}
}

// TestEnforceCodexFastModeStillAppendsEnableOnce guards the split: pulling the
// removal out of enforceCodexFastMode must not change what it produces.
func TestEnforceCodexFastModeStillAppendsEnableOnce(t *testing.T) {
	t.Parallel()

	got := enforceCodexFastMode([]string{"--disable", "fast_mode", "--model", "gpt-5"}, slog.Default())
	want := []string{"--model", "gpt-5", "--enable", "fast_mode"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("enforceCodexFastMode = %v, want %v", got, want)
	}
}
