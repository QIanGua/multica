package service

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	reclaimCheckSchedulePrefix = "mul:claim:runtime:reclaim-schedule:"
	reclaimCheckBackstopPrefix = "mul:claim:runtime:reclaim-backstop:"

	// The backstop remains at or below the PostgreSQL recovery window so a
	// missed dispatch-side Redis write cannot delay recovery beyond one window.
	ReclaimCheckBackstopInterval = claimResponseRecoveryWindow
	// A fresh task's first hint is one recovery window in the future. Keep the
	// containing ZSET alive for another full window so Redis cannot expire the
	// hint at the exact instant it first becomes observable as due.
	ReclaimCheckScheduleTTL = 2 * claimResponseRecoveryWindow
)

// ReclaimCheckCache keeps the batch/singular claim hot paths from issuing a
// stale-dispatch UPDATE when no task can possibly be reclaimed yet.
//
// Each runtime owns a sorted set of task IDs scored by their earliest known
// reclaim time. A separate last-checked timestamp forces a periodic DB check
// for cache loss, pre-deployment rows, or a missed write. Task scores and the
// last-checked timestamp are both application-clock values; PostgreSQL remains
// authoritative for actual eligibility when the query runs. Missing keys and
// Redis errors always mean "check PostgreSQL now", so the cache can only reduce
// load; it is never authoritative for task recovery.
type ReclaimCheckCache struct {
	rdb *redis.Client
}

func NewReclaimCheckCache(rdb *redis.Client) *ReclaimCheckCache {
	if rdb == nil {
		return nil
	}
	return &ReclaimCheckCache{rdb: rdb}
}

func reclaimCheckScheduleKey(runtimeID string) string {
	return reclaimCheckSchedulePrefix + runtimeID
}

func reclaimCheckBackstopKey(runtimeID string) string {
	return reclaimCheckBackstopPrefix + runtimeID
}

func (c *ReclaimCheckCache) bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, emptyClaimRedisTimeout)
}

// DueRuntimeIDs returns only runtimes with a task hint added after their last
// successful DB check and now due, or whose periodic backstop has elapsed. A
// checked hint stays in the ZSET until a real task transition removes/updates it
// or the schedule TTL expires, but it cannot retrigger every poll because its
// score is not newer than the last-checked marker.
//
// A nil cache, missing/malformed backstop, or Redis failure fails open. Batch
// callers treat any due runtime as a reason to query their complete runtime set.
func (c *ReclaimCheckCache) DueRuntimeIDs(ctx context.Context, runtimeIDs []string, now time.Time) []string {
	if len(runtimeIDs) == 0 {
		return nil
	}
	if c == nil {
		return append([]string(nil), runtimeIDs...)
	}

	bctx, cancel := c.bounded(ctx)
	defer cancel()
	validRuntimeIDs := make([]string, 0, len(runtimeIDs))
	backstopKeys := make([]string, 0, len(runtimeIDs))
	for _, runtimeID := range runtimeIDs {
		if runtimeID == "" {
			continue
		}
		validRuntimeIDs = append(validRuntimeIDs, runtimeID)
		backstopKeys = append(backstopKeys, reclaimCheckBackstopKey(runtimeID))
	}
	if len(validRuntimeIDs) == 0 {
		return nil
	}
	backstops, err := c.rdb.MGet(bctx, backstopKeys...).Result()
	if err != nil {
		slog.Warn("reclaim_check_cache: backstop read failed; falling back to DB", "error", err)
		return append([]string(nil), runtimeIDs...)
	}
	if len(backstops) != len(validRuntimeIDs) {
		slog.Warn("reclaim_check_cache: incomplete backstop read; falling back to DB")
		return append([]string(nil), runtimeIDs...)
	}

	nowMillis := now.UnixMilli()
	type scheduleCommand struct {
		runtimeID string
		count     *redis.IntCmd
	}
	due := make([]string, 0, len(validRuntimeIDs))
	pipe := c.rdb.Pipeline()
	scheduleCommands := make([]scheduleCommand, 0, len(validRuntimeIDs))
	for i, runtimeID := range validRuntimeIDs {
		backstop, ok := backstops[i].(string)
		if !ok {
			// A missing or non-string value means this runtime has no trustworthy
			// successful-check marker.
			due = append(due, runtimeID)
			continue
		}
		checkedMillis, err := strconv.ParseInt(backstop, 10, 64)
		if err != nil {
			due = append(due, runtimeID)
			continue
		}
		nextBackstop := checkedMillis + ReclaimCheckBackstopInterval.Milliseconds()
		if nextBackstop < checkedMillis || nextBackstop <= nowMillis {
			due = append(due, runtimeID)
			continue
		}
		scheduleCommands = append(scheduleCommands, scheduleCommand{
			runtimeID: runtimeID,
			count: pipe.ZCount(
				bctx,
				reclaimCheckScheduleKey(runtimeID),
				"("+strconv.FormatInt(checkedMillis, 10),
				strconv.FormatInt(nowMillis, 10),
			),
		})
	}
	if len(scheduleCommands) == 0 {
		return due
	}
	if _, err := pipe.Exec(bctx); err != nil {
		slog.Warn("reclaim_check_cache: schedule read failed; falling back to DB", "error", err)
		return append([]string(nil), runtimeIDs...)
	}
	for _, command := range scheduleCommands {
		count, err := command.count.Result()
		if err != nil {
			slog.Warn("reclaim_check_cache: schedule result failed; falling back to DB", "error", err)
			return append([]string(nil), runtimeIDs...)
		}
		if count > 0 {
			due = append(due, command.runtimeID)
		}
	}
	return due
}

// Track records the earliest known reclaim time for one dispatched task. ZADD
// replaces the task's score after a fresh dispatch/reclaim; multiple tasks on
// one runtime remain independent, avoiding a runtime-level timestamp losing a
// second task's earlier recovery deadline.
func (c *ReclaimCheckCache) Track(ctx context.Context, runtimeID, taskID string, checkAfter time.Time) {
	c.track(ctx, runtimeID, taskID, checkAfter, false)
}

// TrackLater advances an existing task hint only when a prepare-lease extension
// protects it beyond its current recovery deadline. It deliberately does not
// create a missing member: without the initial dispatch deadline, a short lease
// could schedule an early failed check and delay recovery until the next
// backstop. Missing state already fails open through that bounded backstop.
func (c *ReclaimCheckCache) TrackLater(ctx context.Context, runtimeID, taskID string, checkAfter time.Time) {
	c.track(ctx, runtimeID, taskID, checkAfter, true)
}

func (c *ReclaimCheckCache) track(ctx context.Context, runtimeID, taskID string, checkAfter time.Time, onlyLater bool) {
	if c == nil || runtimeID == "" || taskID == "" || checkAfter.IsZero() {
		return
	}
	bctx, cancel := c.bounded(ctx)
	defer cancel()
	key := reclaimCheckScheduleKey(runtimeID)
	pipe := c.rdb.Pipeline()
	member := redis.Z{Score: float64(checkAfter.UnixMilli()), Member: taskID}
	if onlyLater {
		pipe.ZAddArgs(bctx, key, redis.ZAddArgs{XX: true, GT: true, Members: []redis.Z{member}})
	} else {
		pipe.ZAdd(bctx, key, member)
	}
	pipe.Expire(bctx, key, ReclaimCheckScheduleTTL)
	if _, err := pipe.Exec(bctx); err != nil {
		slog.Warn("reclaim_check_cache: track failed; DB fallback remains active", "error", err)
	}
}

// Forget removes a task that can no longer be reclaimed (started, requeued,
// waiting on a local directory, or terminal). Failures are harmless: the stale
// hint can cause one extra DB check but cannot make an ineligible task run.
func (c *ReclaimCheckCache) Forget(ctx context.Context, runtimeID, taskID string) {
	if c == nil || runtimeID == "" || taskID == "" {
		return
	}
	bctx, cancel := c.bounded(ctx)
	defer cancel()
	if err := c.rdb.ZRem(bctx, reclaimCheckScheduleKey(runtimeID), taskID).Err(); err != nil {
		slog.Warn("reclaim_check_cache: forget failed; stale hint will expire", "error", err)
	}
}

// MarkChecked records a successful PostgreSQL reclaim pass for every runtime in
// the machine-level polling set. It deliberately does not delete task hints:
// SKIP LOCKED and runtime-health predicates mean a short result cannot prove
// those tasks no longer need recovery. DueRuntimeIDs ignores scores at or before
// this marker until the bounded backstop elapses, while newer concurrent hints
// can still trigger an earlier check.
func (c *ReclaimCheckCache) MarkChecked(ctx context.Context, runtimeIDs []string, checkedThrough time.Time) {
	if c == nil || len(runtimeIDs) == 0 {
		return
	}
	bctx, cancel := c.bounded(ctx)
	defer cancel()
	checkedMillis := strconv.FormatInt(checkedThrough.UnixMilli(), 10)
	pipe := c.rdb.Pipeline()
	for _, runtimeID := range runtimeIDs {
		if runtimeID == "" {
			continue
		}
		pipe.Set(bctx, reclaimCheckBackstopKey(runtimeID), checkedMillis, ReclaimCheckBackstopInterval)
	}
	if _, err := pipe.Exec(bctx); err != nil {
		slog.Warn("reclaim_check_cache: mark checked failed; falling back to DB", "error", err)
	}
}
