package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

// staleMarkerMinAge is how long a daemon task marker must have been on disk
// before the CLI will consider removing it.
//
// The probe below can only observe the daemon's task count at one instant. A
// daemon that reports zero may be claiming a task right now and about to write
// this very marker, and deleting it in that window would strip the guard off a
// task that is genuinely starting. Nothing orders those two events, so the age
// gate substitutes a bound the race cannot cross: a marker older than this was
// written long before the probe and cannot belong to a task the probe missed.
//
// The cost is that a user who hits a fresh leftover waits, which is acceptable
// because the leftovers this heals are hours to weeks old — the reported one
// sat for five weeks.
const staleMarkerMinAge = time.Hour

// daemonProbeTimeout bounds the whole multi-profile probe. checkDaemonHealthOnPort
// already applies its own 2s per-request timeout; this keeps a machine with many
// profiles from turning a refused command into a long stall.
const daemonProbeTimeout = 10 * time.Second

// daemonHealthProbe is the health lookup the self-heal path uses. It is a
// variable so tests can drive every branch of the decision — reachable and
// idle, reachable and busy, unreachable, too old to report a count — without
// binding real daemon ports, which on a developer machine are already taken by
// the daemons this probe is meant to consult.
var daemonHealthProbe = checkDaemonHealthOnPort

// healStaleDaemonTaskMarker removes the daemon task marker at markerPath when,
// and only when, the CLI can positively prove no daemon-managed task is live on
// this machine. It reports whether the marker was removed.
//
// The marker makes the CLI fail closed: it is the last guard standing between
// an agent subprocess that lost every MULTICA_* env var and a fallback to the
// user's personal token. Removing it is therefore gated on positive evidence
// only — "confirmed absent", never "not found". Every uncertain outcome
// (unreachable daemon, unparseable health payload, a daemon too old to report a
// task count) leaves the marker alone and keeps the command refused, because a
// daemon that cannot be reached is exactly what a crashed daemon looks like,
// and a task orphaned by that crash keeps running: task processes are only
// placed in their own process group (see execenv isolation), never tied to the
// daemon's lifetime.
//
// Callers must only reach this when the marker is the sole daemon signal — any
// MULTICA_* task identity in the environment means the process really is inside
// a task, and no file on disk can override that.
func healStaleDaemonTaskMarker(markerPath string) bool {
	if markerPath == "" {
		return false
	}
	if !markerIsTaskScoped(markerPath) {
		return false
	}
	info, err := os.Stat(markerPath)
	if err != nil || time.Since(info.ModTime()) < staleMarkerMinAge {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), daemonProbeTimeout)
	defer cancel()
	if !noDaemonManagedTaskIsLive(ctx) {
		return false
	}

	if err := os.Remove(markerPath); err != nil {
		return false
	}
	// The daemon creates .multica/ for the marker alone. Remove it when the
	// marker was its only content so an in-place task leaves nothing behind;
	// a non-empty directory (the user's own files, or .multica/project/ from
	// another leftover) fails ENOTEMPTY and is left exactly as it was.
	_ = os.Remove(filepath.Dir(markerPath))
	return true
}

// markerIsTaskScoped reports whether the marker describes one specific task.
//
// Two different markers share this filename. The per-task marker carries the
// agent and issue the task runs for, and dies with that task. The workspaces
// root marker carries neither: it is a permanent, node-wide guard the daemon
// re-creates on every start so a subprocess that escapes above its workdir
// still fails closed. Only the first is ever a leftover; deleting the second
// would silently disarm that guard for the whole tree, so anything without both
// identifiers is out of scope here regardless of what the daemon probe says.
func markerIsTaskScoped(markerPath string) bool {
	data, err := os.ReadFile(markerPath)
	if err != nil {
		return false
	}
	var marker struct {
		ManagedBy string `json:"managed_by"`
		AgentID   string `json:"agent_id"`
		IssueID   string `json:"issue_id"`
	}
	if json.Unmarshal(data, &marker) != nil {
		return false
	}
	return marker.ManagedBy == execenv.TaskContextMarkerManagedBy &&
		marker.AgentID != "" && marker.IssueID != ""
}

// noDaemonManagedTaskIsLive reports whether EVERY profile's daemon on this
// machine answered and reported zero active tasks.
//
// A profile whose daemon does not answer is not evidence of absence: that is
// also what a daemon crashed mid-task looks like, and its orphaned task keeps
// running with the marker as its only remaining guard. So a single silent or
// unparseable profile makes the whole answer "unknown", and the caller must
// keep failing closed. This is deliberately stricter than "the daemon I would
// have talked to says zero" — a task claimed under one profile can be running
// in a directory a command resolved to another profile touches.
func noDaemonManagedTaskIsLive(ctx context.Context) bool {
	named, err := knownProfiles()
	if err != nil {
		// The profile list is how we know which ports to probe. Without it we
		// cannot claim to have checked them all.
		return false
	}
	// The empty string is the default profile, which knownProfiles omits.
	for _, profile := range append([]string{""}, named...) {
		count, ok := probeDaemonActiveTasks(ctx, healthPortForProfile(profile))
		if !ok || count != 0 {
			return false
		}
	}
	return true
}

// probeDaemonActiveTasks returns the active task count reported by the daemon
// on port, and whether that number could be established at all.
//
// active_task_count is the only count valid here. It spans the whole claimed
// lifecycle including preparation, which is precisely when a marker exists but
// no provider has launched; running_task_count omits that window and the daemon
// documents it as unusable for safety barriers.
//
// The second return is why this exists alongside daemonActiveTaskCount, which
// reports a missing field as 0. That reading is right for its caller — a
// restart barrier that errs toward "go ahead" — and wrong here, where "the
// daemon never told us" must not be read as "there is nothing running".
func probeDaemonActiveTasks(ctx context.Context, port int) (int64, bool) {
	health := daemonHealthProbe(ctx, port)
	if !daemonAlive(health) {
		return 0, false
	}
	// checkDaemonHealthOnPort decodes into map[string]any, so JSON numbers
	// arrive as float64. A daemon predating the field reports nothing, which is
	// unknown rather than zero.
	raw, ok := health["active_task_count"].(float64)
	if !ok {
		return 0, false
	}
	return int64(raw), true
}
