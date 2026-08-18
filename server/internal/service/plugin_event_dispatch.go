package service

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/plugincontract"
)

// Event-triggered hooks.
//
// The rule this file exists to keep: an event hook NEVER blocks the host. The
// event bus is synchronous — Bus.Publish runs its listeners inline, on the
// goroutine of whatever request produced the event — so a listener that dialled
// a third-party endpoint would put an outside server on the critical path of
// creating an issue. Everything here therefore hands off to a bounded worker
// pool and returns immediately.
//
// The same reasoning is why the agent execution path has no hook at all: a hook
// that must run before or after every agent turn is a third party holding the
// product's main loop open. Agents reach hooks in PR 4 by choosing to call one
// as a tool, which is a call they can decline.

const (
	// Queue depth. Full means events are arriving faster than endpoints can
	// answer; the overflow is dropped and counted rather than queued forever,
	// because an unbounded queue turns a slow plugin into a memory leak.
	dispatchQueueDepth = 512
	dispatchWorkers    = 4
)

// PluginEventDispatcher fans domain events out to the hooks that asked for them.
type PluginEventDispatcher struct {
	service *PluginService
	queue   chan dispatchJob
	wg      sync.WaitGroup
	stop    chan struct{}
	once    sync.Once

	// dropped counts events shed under backpressure, surfaced for triage.
	mu      sync.Mutex
	dropped int
}

type dispatchJob struct {
	installation db.PluginInstallation
	hook         plugincontract.Hook
	eventType    string
	issueID      pgtype.UUID
	payload      any
}

func NewPluginEventDispatcher(service *PluginService) *PluginEventDispatcher {
	dispatcher := &PluginEventDispatcher{
		service: service,
		queue:   make(chan dispatchJob, dispatchQueueDepth),
		stop:    make(chan struct{}),
	}
	for i := 0; i < dispatchWorkers; i++ {
		dispatcher.wg.Add(1)
		go dispatcher.work()
	}
	return dispatcher
}

// Dispatch is what the bus listener calls. It must return promptly: the caller
// is a live request that has already done its real work.
//
// Note it takes no context from that request. Tying an outbound hook to the
// request that triggered it would cancel the hook the moment the browser got
// its response, which is exactly when the hook is only just starting.
func (d *PluginEventDispatcher) Dispatch(eventType, workspaceID string, issueID pgtype.UUID, payload any) {
	if d == nil || d.service == nil || workspaceID == "" {
		return
	}
	parsedWorkspace, err := parseUUIDValue(workspaceID)
	if err != nil {
		return
	}

	// The lookup is cheap and local, but it is still a database read, so it
	// happens on a worker rather than on the publishing goroutine.
	select {
	case d.queue <- dispatchJob{
		installation: db.PluginInstallation{WorkspaceID: parsedWorkspace},
		eventType:    eventType,
		issueID:      issueID,
		payload:      payload,
	}:
	default:
		d.mu.Lock()
		d.dropped++
		dropped := d.dropped
		d.mu.Unlock()
		slog.Warn("plugins: event dispatch queue full, dropping event", "event_type", eventType, "dropped_total", dropped)
	}
}

// Dropped reports how many events were shed under backpressure.
func (d *PluginEventDispatcher) Dropped() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dropped
}

func (d *PluginEventDispatcher) work() {
	defer d.wg.Done()
	for {
		select {
		case <-d.stop:
			return
		case job := <-d.queue:
			d.runGuarded(job)
		}
	}
}

// runGuarded contains a panic to the delivery that caused it.
//
// Bus.Publish recovers panics in its listeners so one bad handler cannot take
// down the request that published. Moving delivery onto a worker goroutine
// steps outside that protection, and a panic on a bare goroutine is not a
// failed hook but a dead process. Restoring the guarantee is the cost of the
// hand-off, not an optional extra.
func (d *PluginEventDispatcher) runGuarded(job dispatchJob) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("plugins: panic while delivering an event hook",
				"event_type", job.eventType, "recovered", recovered)
		}
	}()
	d.run(job)
}

// run resolves which hooks want this event and calls each one.
func (d *PluginEventDispatcher) run(job dispatchJob) {
	if d.service.Queries == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	installations, err := d.service.Queries.ListWorkspacePluginInstallations(ctx, job.installation.WorkspaceID)
	if err != nil {
		slog.Warn("plugins: event dispatch could not list installations", "error", err)
		return
	}
	for _, installation := range installations {
		if !installation.Enabled {
			continue
		}
		manifest, err := ParseInstallationManifest(installation)
		if err != nil {
			continue
		}
		for _, hook := range manifest.Contributes.Hooks {
			if !HookAllowsTrigger(hook, plugincontract.TriggerEvent) || !hookWantsEvent(hook, job.eventType) {
				continue
			}
			d.deliver(ctx, installation, hook, job)
		}
	}
}

func hookWantsEvent(hook plugincontract.Hook, eventType string) bool {
	for _, declared := range hook.Events {
		if declared == eventType {
			return true
		}
	}
	return false
}

// deliver runs one hook with the event retry schedule.
func (d *PluginEventDispatcher) deliver(ctx context.Context, installation db.PluginInstallation, hook plugincontract.Hook, job dispatchJob) {
	// A hook whose endpoint has been failing is not retried on every event.
	// Without this, an endpoint that has been down for an hour receives one
	// doomed request per workspace event, forever.
	if d.service.HookBreakerOpen(ctx, installation.ID, hook.Key) {
		slog.Info("plugins: hook circuit open, skipping event", "hook", hook.Key, "event_type", job.eventType)
		return
	}

	invocation := HookInvocation{
		Installation: installation,
		Hook:         hook,
		Trigger:      plugincontract.TriggerEvent,
		EventType:    job.eventType,
		// An event has no person behind it. Writes it produces are the
		// plugin's own, attributed to the installation.
		Actor:   HookActor{Type: "plugin", ID: installation.ID},
		IssueID: job.issueID,
		Input:   job.payload,
	}

	for attempt := 1; attempt <= hookEventAttempts; attempt++ {
		_, err := d.service.InvokeHook(ctx, invocation, attempt)
		if err == nil {
			return
		}
		// A refusal is a decision, not an outage: retrying a hook that is
		// disabled, out of scope or rate limited just burns the budget.
		if hookFailureStatus(err) == "refused" {
			slog.Info("plugins: event hook refused", "hook", hook.Key, "error", redactHookError(err))
			return
		}
		if attempt == hookEventAttempts {
			slog.Warn("plugins: event hook failed after retries", "hook", hook.Key, "event_type", job.eventType, "error", redactHookError(err))
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(attempt) * hookEventBackoff):
		}
	}
}

// Close stops the workers. Safe to call more than once.
func (d *PluginEventDispatcher) Close() {
	if d == nil {
		return
	}
	d.once.Do(func() {
		close(d.stop)
		d.wg.Wait()
	})
}
