package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// serveHealthAs stands up a stub daemon on the health port that `profile`
// hashes to, answering with the identity `reports` claims. A nil reports map
// means a pre-#6694 daemon: alive, but unable to say who it is.
func serveHealthAs(t *testing.T, profile string, reports map[string]any) int {
	t.Helper()
	port := healthPortForProfile(profile)
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Skipf("health port %d for profile %q is busy on this machine: %v", port, profile, err)
	}

	body := map[string]any{"status": "running", "pid": 4242, "uptime": "1h0m0s", "cli_version": "v0.4.23"}
	for k, v := range reports {
		body[k] = v
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(body)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return port
}

func TestDaemonIdentityMismatch(t *testing.T) {
	cases := []struct {
		name    string
		health  map[string]any
		profile string
		wantErr bool
	}{
		{"same named profile", map[string]any{"profile": "dev"}, "dev", false},
		{"default profile identifying itself", map[string]any{"profile": ""}, "", false},
		{"different profile on our port", map[string]any{"profile": "dev-b"}, "dev-a", true},
		{"our port serving the default daemon", map[string]any{"profile": ""}, "dev", true},
		{"we asked for default, a named daemon answered", map[string]any{"profile": "dev"}, "", true},
		// A daemon that predates the field cannot prove anything; refusing
		// would break lifecycle commands against a merely older daemon.
		{"older daemon omits the field", map[string]any{"pid": 1.0}, "dev", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := daemonIdentityMismatch(tc.health, tc.profile, 19589)
			var mismatch *daemonProfileMismatchError
			if got := errors.As(err, &mismatch); got != tc.wantErr {
				t.Fatalf("daemonIdentityMismatch = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestDaemonProfileMismatchErrorNamesBothSides(t *testing.T) {
	err := &daemonProfileMismatchError{Want: "desktop-localhost-18310", Got: "desktop-localhost-18130", Port: 19589}
	msg := err.Error()
	for _, want := range []string{"19589", "desktop-localhost-18130", "desktop-localhost-18310", "same health port"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q should mention %q so the user can tell the two apart", msg, want)
		}
	}

	// The default profile has no name to print; "" would read as a bug.
	def := &daemonProfileMismatchError{Want: "dev", Got: "", Port: 19514}
	if !strings.Contains(def.Error(), "the default profile") {
		t.Errorf("error %q should name the default profile in words", def.Error())
	}
}

// The collision is only dangerous because these commands act on whatever
// answered: stop reads the PID off it and kills it, restart kills it and takes
// its port. Both must refuse before touching anything.
func TestDaemonLifecycleRefusesForeignDaemon(t *testing.T) {
	cases := []struct {
		name string
		run  func(*cobra.Command) error
	}{
		{"stop", func(c *cobra.Command) error { return runDaemonStop(c, nil) }},
		{"restart", func(c *cobra.Command) error { return runDaemonRestart(c, nil) }},
		{"start", func(c *cobra.Command) error { return runDaemonBackground(c) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearDaemonTaskEnv(t)
			mkProfiles(t, "collide-ab", "collide-ba")
			// Same bytes, so the same hashed port: the shape of a real
			// collision without depending on a lucky pair of names.
			serveHealthAs(t, "collide-ab", map[string]any{"profile": "collide-ab"})

			cmd := daemonStatusCmdFor(t, "collide-ba", "")
			cmd.Flags().Bool("foreground", false, "")
			err := tc.run(cmd)

			var mismatch *daemonProfileMismatchError
			if !errors.As(err, &mismatch) {
				t.Fatalf("daemon %s = %v, want a refusal naming the port collision", tc.name, err)
			}
			if mismatch.Got != "collide-ab" || mismatch.Want != "collide-ba" {
				t.Fatalf("mismatch = %+v, want got=collide-ab want=collide-ba", mismatch)
			}
		})
	}
}

// A daemon too old to identify itself must keep working: stop still reports
// against it rather than refusing.
func TestDaemonStopAcceptsDaemonWithoutProfileField(t *testing.T) {
	clearDaemonTaskEnv(t)
	mkProfiles(t, "legacy")
	serveHealthAs(t, "legacy", nil)

	err := runDaemonStop(daemonStatusCmdFor(t, "legacy", ""), nil)
	var mismatch *daemonProfileMismatchError
	if errors.As(err, &mismatch) {
		t.Fatal("a daemon that predates the profile field must not be refused")
	}
}

func TestDaemonStatusReportsPortConflictInsteadOfClaimingItsOwn(t *testing.T) {
	t.Run("table says stopped and names the occupant", func(t *testing.T) {
		clearDaemonTaskEnv(t)
		mkProfiles(t, "collide-ab", "collide-ba")
		port := serveHealthAs(t, "collide-ab", map[string]any{"profile": "collide-ab"})

		out, err := captureStdout(t, func() error {
			return runDaemonStatus(daemonStatusCmdFor(t, "collide-ba", ""), nil)
		})
		if err != nil {
			t.Fatalf("runDaemonStatus = %v, want nil", err)
		}
		if !strings.Contains(out, "stopped") {
			t.Errorf("stdout = %q, want collide-ba reported as stopped — its daemon is not running", out)
		}
		if strings.Contains(out, "running (pid 4242") {
			t.Errorf("stdout = %q, must not report another profile's daemon as this one's", out)
		}
		if !strings.Contains(out, "collide-ab") || !strings.Contains(out, fmt.Sprint(port)) {
			t.Errorf("stdout = %q, should name the occupying profile and the shared port", out)
		}
	})

	t.Run("json keeps status stopped and adds the conflict", func(t *testing.T) {
		clearDaemonTaskEnv(t)
		mkProfiles(t, "collide-ab", "collide-ba")
		serveHealthAs(t, "collide-ab", map[string]any{"profile": "collide-ab"})

		out, err := captureStdout(t, func() error {
			return runDaemonStatus(daemonStatusCmdFor(t, "collide-ba", "json"), nil)
		})
		if err != nil {
			t.Fatalf("runDaemonStatus = %v, want nil", err)
		}
		var payload struct {
			Status       string `json:"status"`
			PortConflict struct {
				Port    int    `json:"port"`
				Profile string `json:"profile"`
			} `json:"port_conflict"`
		}
		if err := json.Unmarshal([]byte(out), &payload); err != nil {
			t.Fatalf("stdout is not valid JSON: %v\n%s", err, out)
		}
		// Existing `jq -r .status` callers must keep reading a truthful value.
		if payload.Status != "stopped" {
			t.Errorf("status = %q, want stopped", payload.Status)
		}
		if payload.PortConflict.Profile != "collide-ab" {
			t.Errorf("port_conflict.profile = %q, want collide-ab", payload.PortConflict.Profile)
		}
	})
}

func TestDaemonStatusShowsWhoManagesTheDaemon(t *testing.T) {
	t.Run("desktop-managed daemon says so", func(t *testing.T) {
		clearDaemonTaskEnv(t)
		mkProfiles(t, "desk-managed-test")
		serveHealthAs(t, "desk-managed-test", map[string]any{
			"profile": "desk-managed-test", "launched_by": "desktop",
		})

		out, err := captureStdout(t, func() error {
			return runDaemonStatus(daemonStatusCmdFor(t, "desk-managed-test", ""), nil)
		})
		if err != nil {
			t.Fatalf("runDaemonStatus = %v, want nil", err)
		}
		if !strings.Contains(out, "Managed by") || !strings.Contains(out, "Desktop") {
			t.Errorf("stdout = %q, want it to say the Desktop app manages this daemon", out)
		}
	})

	t.Run("standalone daemon output is unchanged", func(t *testing.T) {
		clearDaemonTaskEnv(t)
		mkProfiles(t, "standalone")
		serveHealthAs(t, "standalone", map[string]any{"profile": "standalone"})

		out, err := captureStdout(t, func() error {
			return runDaemonStatus(daemonStatusCmdFor(t, "standalone", ""), nil)
		})
		if err != nil {
			t.Fatalf("runDaemonStatus = %v, want nil", err)
		}
		if strings.Contains(out, "Managed by") {
			t.Errorf("stdout = %q, a standalone daemon must not grow a Managed by row", out)
		}
	})
}
