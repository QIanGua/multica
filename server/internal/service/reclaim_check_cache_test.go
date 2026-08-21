package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestReclaimCheckCache_NilFallsBackToEveryRuntime(t *testing.T) {
	var cache *ReclaimCheckCache
	runtimes := []string{"rt-a", "rt-b"}
	due := cache.DueRuntimeIDs(context.Background(), runtimes, time.Now())
	if len(due) != len(runtimes) || due[0] != runtimes[0] || due[1] != runtimes[1] {
		t.Fatalf("nil cache due runtimes = %v, want %v", due, runtimes)
	}
	cache.Track(context.Background(), "rt-a", "task-a", time.Now())
	cache.TrackLater(context.Background(), "rt-a", "task-a", time.Now())
	cache.Forget(context.Background(), "rt-a", "task-a")
	cache.MarkChecked(context.Background(), runtimes, time.Now())
}

func TestNewReclaimCheckCache_NilRedisReturnsNil(t *testing.T) {
	if cache := NewReclaimCheckCache(nil); cache != nil {
		t.Fatalf("NewReclaimCheckCache(nil) = %#v, want nil", cache)
	}
}

func TestReclaimCheckCache_RedisFailureFallsBackToEveryRuntime(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"})
	if err := rdb.Close(); err != nil {
		t.Fatalf("close Redis client: %v", err)
	}
	cache := NewReclaimCheckCache(rdb)
	runtimes := []string{"rt-a", "rt-b"}
	due := cache.DueRuntimeIDs(context.Background(), runtimes, time.Now())
	if len(due) != len(runtimes) || due[0] != runtimes[0] || due[1] != runtimes[1] {
		t.Fatalf("failed Redis due runtimes = %v, want %v", due, runtimes)
	}
}

func TestReclaimCheckCache_ProductionWindowHintOutlivesDeadline(t *testing.T) {
	rdb := newRedisTestClient(t)
	cache := NewReclaimCheckCache(rdb)
	ctx := context.Background()
	now := time.Now()
	hint := now.Add(claimResponseRecoveryWindow)

	// Put the backstop after the task hint so this test exercises the schedule,
	// not the periodic fallback, at the production 90-second distance.
	cache.MarkChecked(ctx, []string{"rt-a"}, now.Add(30*time.Second))
	cache.Track(ctx, "rt-a", "task-a", hint)

	ttl, err := rdb.PTTL(ctx, reclaimCheckScheduleKey("rt-a")).Result()
	if err != nil {
		t.Fatalf("schedule PTTL: %v", err)
	}
	if ttl <= claimResponseRecoveryWindow {
		t.Fatalf("schedule TTL = %v, must strictly outlive %v hint distance", ttl, claimResponseRecoveryWindow)
	}
	if due := cache.DueRuntimeIDs(ctx, []string{"rt-a"}, hint.Add(-time.Millisecond)); len(due) != 0 {
		t.Fatalf("task must not be checked before its recovery time, got %v", due)
	}
	if due := cache.DueRuntimeIDs(ctx, []string{"rt-a"}, hint.Add(time.Millisecond)); len(due) != 1 {
		t.Fatalf("production-window hint must remain observable when due, got %v", due)
	}
}

func TestReclaimCheckCache_CheckedHintIsRetainedWithoutRetriggering(t *testing.T) {
	rdb := newRedisTestClient(t)
	cache := NewReclaimCheckCache(rdb)
	ctx := context.Background()
	now := time.Now()

	cache.MarkChecked(ctx, []string{"rt-a"}, now)
	cache.Track(ctx, "rt-a", "task-a", now.Add(10*time.Second))
	if due := cache.DueRuntimeIDs(ctx, []string{"rt-a"}, now.Add(11*time.Second)); len(due) != 1 {
		t.Fatalf("task-a should trigger its first check, got %v", due)
	}
	cache.MarkChecked(ctx, []string{"rt-a"}, now.Add(11*time.Second))
	if due := cache.DueRuntimeIDs(ctx, []string{"rt-a"}, now.Add(12*time.Second)); len(due) != 0 {
		t.Fatalf("already-checked hint must not retrigger the next poll, got %v", due)
	}
	if _, err := rdb.ZScore(ctx, reclaimCheckScheduleKey("rt-a"), "task-a").Result(); err != nil {
		t.Fatalf("successful check must retain task hint: %v", err)
	}

	// A new task added after the check can still trigger before the backstop,
	// even though the older retained member remains first in score order.
	cache.Track(ctx, "rt-a", "task-b", now.Add(20*time.Second))
	if due := cache.DueRuntimeIDs(ctx, []string{"rt-a"}, now.Add(21*time.Second)); len(due) != 1 {
		t.Fatalf("newer task hint should override the backstop, got %v", due)
	}
}

func TestReclaimCheckCache_TrackLaterAndForget(t *testing.T) {
	rdb := newRedisTestClient(t)
	cache := NewReclaimCheckCache(rdb)
	ctx := context.Background()
	now := time.Now()

	cache.Track(ctx, "rt-a", "task-a", now.Add(claimResponseRecoveryWindow))
	cache.TrackLater(ctx, "rt-a", "task-a", now.Add(prepareLeaseDuration))
	score, err := rdb.ZScore(ctx, reclaimCheckScheduleKey("rt-a"), "task-a").Result()
	if err != nil {
		t.Fatalf("read task score after shorter lease: %v", err)
	}
	if got := int64(score); got != now.Add(claimResponseRecoveryWindow).UnixMilli() {
		t.Fatalf("shorter lease moved hint to %d, want %d", got, now.Add(claimResponseRecoveryWindow).UnixMilli())
	}

	later := now.Add(claimResponseRecoveryWindow + prepareLeaseDuration)
	cache.TrackLater(ctx, "rt-a", "task-a", later)
	score, err = rdb.ZScore(ctx, reclaimCheckScheduleKey("rt-a"), "task-a").Result()
	if err != nil {
		t.Fatalf("read task score after later lease: %v", err)
	}
	if got := int64(score); got != later.UnixMilli() {
		t.Fatalf("later lease left hint at %d, want %d", got, later.UnixMilli())
	}

	cache.Forget(ctx, "rt-a", "task-a")
	if _, err := rdb.ZScore(ctx, reclaimCheckScheduleKey("rt-a"), "task-a").Result(); !errors.Is(err, redis.Nil) {
		t.Fatalf("forgotten task score error = %v, want redis.Nil", err)
	}
	cache.TrackLater(ctx, "rt-a", "missing-task", later)
	if _, err := rdb.ZScore(ctx, reclaimCheckScheduleKey("rt-a"), "missing-task").Result(); !errors.Is(err, redis.Nil) {
		t.Fatalf("lease-only missing task score error = %v, want redis.Nil", err)
	}
}

func TestReclaimCheckCache_CollectionCheckAlignsBackstops(t *testing.T) {
	rdb := newRedisTestClient(t)
	cache := NewReclaimCheckCache(rdb)
	ctx := context.Background()
	now := time.Now()
	runtimes := []string{"rt-a", "rt-b"}

	cache.MarkChecked(ctx, []string{"rt-a"}, now)
	cache.MarkChecked(ctx, []string{"rt-b"}, now.Add(30*time.Second))
	if due := cache.DueRuntimeIDs(ctx, runtimes, now.Add(91*time.Second)); len(due) != 1 || due[0] != "rt-a" {
		t.Fatalf("staggered backstops due = %v, want [rt-a]", due)
	}

	// ClaimTasksForRuntimes queries the complete set when any member is due and
	// records that successful pass for the complete set as well.
	cache.MarkChecked(ctx, runtimes, now.Add(91*time.Second))
	if due := cache.DueRuntimeIDs(ctx, runtimes, now.Add(150*time.Second)); len(due) != 0 {
		t.Fatalf("collection backstops drifted after joint check: %v", due)
	}
	if due := cache.DueRuntimeIDs(ctx, runtimes, now.Add(182*time.Second)); len(due) != 2 {
		t.Fatalf("aligned collection backstops due = %v, want both runtimes", due)
	}
}
