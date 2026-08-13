package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/multica-ai/multica/server/internal/cli"
)

// leftoverDaemonTaskMarkerPath returns the workdir marker path when a marker is
// the ONLY reason the CLI considers itself inside a daemon-managed task, and ""
// otherwise.
//
// Any MULTICA_* task identity in the environment means the daemon really did
// launch this process, so the marker is doing its job and none of the
// leftover-specific handling applies. MULTICA_DAEMON_PORT is treated the same
// way even though it is not sufficient on its own for identity: it still says a
// daemon environment is present, which is not a leftover's fingerprint.
func leftoverDaemonTaskMarkerPath() string {
	if inAgentExecutionContext() ||
		strings.TrimSpace(os.Getenv(cli.TaskConfigRootEnv)) != "" ||
		strings.TrimSpace(os.Getenv("MULTICA_DAEMON_PORT")) != "" {
		return ""
	}
	return daemonTaskContextMarkerPath()
}

// leftoverMarkerSuffix renders the shared tail both refusal paths append when a
// workdir marker is the only daemon signal: newAPIClient for API commands and
// requireHumanLocalCommand for the local daemon/profile commands.
//
// Both refusals have the same cause and the same remedy, so they say the same
// thing. Keeping the wording in one place is what stops the two paths from
// drifting again — before MUL-6132 only the API path named the file, so whether
// a user could act on the error depended on which command they happened to run
// first (MUL-6132 review).
//
// The CLI deliberately stops at naming the file rather than deleting it.
// Removing the marker is removing a fail-closed guard, and no check the CLI can
// run proves it is safe to: the daemon increments its active task count only
// after a claim returns, so a probe can read zero for a task that is already
// claimed and about to write this very marker. Retirement therefore belongs to
// the daemon, which can serialize it against its own claim path.
func leftoverMarkerSuffix(markerPath string) string {
	return fmt.Sprintf("; detected a daemon task marker at %s — if you are not running inside an agent task this is likely a leftover, remove it and retry", markerPath)
}
