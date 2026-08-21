package service

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	reclaimCheckSchedulePrefix = "mul:claim:runtime:reclaim-schedule:"
	reclaimCheckBackstopPrefix = "mul:claim:runtime:reclaim-backstop:"
)

// ReclaimCheckCacheTTL is both the steady-state fallback cadence and the TTL
// for reclaim scheduling hints. It must not exceed the claim response recovery
// window: if a dispatch-side Redis write is lost, the prior backstop expires no
// later than the newly dispatched task first becomes reclaimable.
const ReclaimCheckCacheTTL = claimResponseRecoveryWindow

// ReclaimCheckCache keeps the batch/singular claim hot paths from issuing a
// stale-dispatch UPDATE when no task can possibly be reclaimed yet.
//
// Each runtime owns a sorted set of task IDs scored by their earliest known
// reclaim time. A separate backstop timestamp forces a periodic DB check for
// cache loss, pre-deployment rows, or a missed write. Missing keys and Redis
// errors always mean "check PostgreSQL now", so the cache can only reduce load;
// it is never authoritative for task recovery.
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

// DueRuntimeIDs returns only runtimes whose earliest task hint or periodic
// backstop is due. A nil cache, missing backstop, malformed value, or Redis
// failure returns every input runtime so callers preserve the pre-cache DB path.
func (c *ReclaimCheckCache) DueRuntimeIDs(ctx context.Context, runtimeIDs []string, now time.Time) []string {
	if len(runtimeIDs) == 0 {
		return nil
	}
	if c == nil {
		return append([]string(nil), runtimeIDs...)
	}

	bctx, cancel := c.bounded(ctx)
	defer cancel()
	type runtimeCommands struct {
		runtimeID string
		schedule  *redis.ZSliceCmd
		backstop  *redis.StringCmd
	}
	pipe := c.rdb.Pipeline()
	commands := make([]runtimeCommands, 0, len(runtimeIDs))
	for _, runtimeID := range runtimeIDs {
		if runtimeID == "" {
			continue
		}
		commands = append(commands, runtimeCommands{
			runtimeID: runtimeID,
			schedule:  pipe.ZRangeWithScores(bctx, reclaimCheckScheduleKey(runtimeID), 0, 0),
			backstop:  pipe.Get(bctx, reclaimCheckBackstopKey(runtimeID)),
		})
	}
	if _, err := pipe.Exec(bctx); err != nil && !errors.Is(err, redis.Nil) {
		slog.Warn("reclaim_check_cache: read failed; falling back to DB", "error", err)
		return append([]string(nil), runtimeIDs...)
	}

	nowMillis := now.UnixMilli()
	due := make([]string, 0, len(commands))
	for _, command := range commands {
		scheduled, err := command.schedule.Result()
		if err != nil {
			slog.Warn("reclaim_check_cache: schedule read failed; falling back to DB", "error", err)
			return append([]string(nil), runtimeIDs...)
		}
		if len(scheduled) > 0 && int64(scheduled[0].Score) <= nowMillis {
			due = append(due, command.runtimeID)
			continue
		}

		backstop, err := command.backstop.Result()
		if errors.Is(err, redis.Nil) {
			due = append(due, command.runtimeID)
			continue
		}
		if err != nil {
			slog.Warn("reclaim_check_cache: backstop read failed; falling back to DB", "error", err)
			return append([]string(nil), runtimeIDs...)
		}
		backstopMillis, err := strconv.ParseInt(backstop, 10, 64)
		if err != nil || backstopMillis <= nowMillis {
			due = append(due, command.runtimeID)
		}
	}
	return due
}

// Track records the earliest known reclaim time for one dispatched task. ZADD
// updates the task's score when a prepare lease is extended; multiple tasks on
// one runtime remain independent, avoiding a runtime-level timestamp losing a
// second task's earlier recovery deadline.
func (c *ReclaimCheckCache) Track(ctx context.Context, runtimeID, taskID string, checkAfter time.Time) {
	if c == nil || runtimeID == "" || taskID == "" || checkAfter.IsZero() {
		return
	}
	bctx, cancel := c.bounded(ctx)
	defer cancel()
	key := reclaimCheckScheduleKey(runtimeID)
	pipe := c.rdb.Pipeline()
	pipe.ZAdd(bctx, key, redis.Z{Score: float64(checkAfter.UnixMilli()), Member: taskID})
	pipe.Expire(bctx, key, ReclaimCheckCacheTTL)
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

// MarkChecked records a successful PostgreSQL reclaim pass. When the UPDATE
// returned fewer rows than its LIMIT, it exhausted currently eligible rows, so
// task hints due before the query began can be discarded. Hints added or moved
// by a concurrent dispatch/lease extension have a later score and survive.
func (c *ReclaimCheckCache) MarkChecked(ctx context.Context, runtimeIDs []string, checkedThrough time.Time, exhausted bool) {
	if c == nil || len(runtimeIDs) == 0 {
		return
	}
	bctx, cancel := c.bounded(ctx)
	defer cancel()
	nextBackstop := checkedThrough.Add(ReclaimCheckCacheTTL).UnixMilli()
	maxChecked := strconv.FormatInt(checkedThrough.UnixMilli(), 10)
	pipe := c.rdb.Pipeline()
	for _, runtimeID := range runtimeIDs {
		if runtimeID == "" {
			continue
		}
		if exhausted {
			pipe.ZRemRangeByScore(bctx, reclaimCheckScheduleKey(runtimeID), "-inf", maxChecked)
		}
		pipe.Set(bctx, reclaimCheckBackstopKey(runtimeID), strconv.FormatInt(nextBackstop, 10), ReclaimCheckCacheTTL)
		pipe.Expire(bctx, reclaimCheckScheduleKey(runtimeID), ReclaimCheckCacheTTL)
	}
	if _, err := pipe.Exec(bctx); err != nil {
		slog.Warn("reclaim_check_cache: mark checked failed; falling back to DB", "error", err)
	}
}
