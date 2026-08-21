package service

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/redis/go-redis/v9"
)

func TestTaskReclaimCheckAfter(t *testing.T) {
	dispatchedAt := time.Date(2026, time.August, 21, 4, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		task db.AgentTaskQueue
		want time.Time
	}{
		{
			name: "missing dispatch timestamp has no schedule",
			task: db.AgentTaskQueue{},
		},
		{
			name: "recovery window",
			task: db.AgentTaskQueue{
				DispatchedAt: pgtype.Timestamptz{Time: dispatchedAt, Valid: true},
			},
			want: dispatchedAt.Add(claimResponseRecoveryWindow),
		},
		{
			name: "later prepare lease",
			task: db.AgentTaskQueue{
				DispatchedAt:          pgtype.Timestamptz{Time: dispatchedAt, Valid: true},
				PrepareLeaseExpiresAt: pgtype.Timestamptz{Time: dispatchedAt.Add(2 * time.Minute), Valid: true},
			},
			want: dispatchedAt.Add(2 * time.Minute),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := taskReclaimCheckAfter(tt.task); !got.Equal(tt.want) {
				t.Fatalf("taskReclaimCheckAfter() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReclaimCheckCache_NilFallsBackToEveryRuntime(t *testing.T) {
	var cache *ReclaimCheckCache
	runtimes := []string{"rt-a", "rt-b"}
	due := cache.DueRuntimeIDs(context.Background(), runtimes, time.Now())
	if len(due) != len(runtimes) || due[0] != runtimes[0] || due[1] != runtimes[1] {
		t.Fatalf("nil cache due runtimes = %v, want %v", due, runtimes)
	}
	cache.Track(context.Background(), "rt-a", "task-a", time.Now())
	cache.Forget(context.Background(), "rt-a", "task-a")
	cache.MarkChecked(context.Background(), runtimes, time.Now(), true)
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

func TestReclaimCheckCache_BackstopAndTaskSchedule(t *testing.T) {
	rdb := newRedisTestClient(t)
	cache := NewReclaimCheckCache(rdb)
	ctx := context.Background()
	now := time.Now()

	if due := cache.DueRuntimeIDs(ctx, []string{"rt-a"}, now); len(due) != 1 {
		t.Fatalf("missing cache must fall back to DB, got %v", due)
	}
	cache.MarkChecked(ctx, []string{"rt-a"}, now, true)
	if due := cache.DueRuntimeIDs(ctx, []string{"rt-a"}, now.Add(time.Second)); len(due) != 0 {
		t.Fatalf("fresh backstop should skip DB, got %v", due)
	}

	cache.Track(ctx, "rt-a", "task-a", now.Add(10*time.Second))
	if due := cache.DueRuntimeIDs(ctx, []string{"rt-a"}, now.Add(9*time.Second)); len(due) != 0 {
		t.Fatalf("task must not be checked before its recovery time, got %v", due)
	}
	if due := cache.DueRuntimeIDs(ctx, []string{"rt-a"}, now.Add(11*time.Second)); len(due) != 1 {
		t.Fatalf("task recovery time must override the later backstop, got %v", due)
	}
}

func TestReclaimCheckCache_LeaseMoveForgetAndExhaustedCleanup(t *testing.T) {
	rdb := newRedisTestClient(t)
	cache := NewReclaimCheckCache(rdb)
	ctx := context.Background()
	now := time.Now()

	cache.MarkChecked(ctx, []string{"rt-a"}, now, true)
	cache.Track(ctx, "rt-a", "task-a", now.Add(5*time.Second))
	cache.Track(ctx, "rt-a", "task-b", now.Add(20*time.Second))
	// A prepare-lease extension moves the same task rather than adding a second
	// member, so task-b remains the earliest hint.
	cache.Track(ctx, "rt-a", "task-a", now.Add(30*time.Second))
	if due := cache.DueRuntimeIDs(ctx, []string{"rt-a"}, now.Add(21*time.Second)); len(due) != 1 {
		t.Fatalf("task-b should remain due after task-a lease extension, got %v", due)
	}

	cache.Forget(ctx, "rt-a", "task-b")
	if due := cache.DueRuntimeIDs(ctx, []string{"rt-a"}, now.Add(21*time.Second)); len(due) != 0 {
		t.Fatalf("forgotten task must no longer trigger a check, got %v", due)
	}

	cache.Track(ctx, "rt-a", "old-task", now.Add(-time.Second))
	cache.Track(ctx, "rt-a", "future-task", now.Add(40*time.Second))
	cache.MarkChecked(ctx, []string{"rt-a"}, now, true)
	if score, err := rdb.ZScore(ctx, reclaimCheckScheduleKey("rt-a"), "old-task").Result(); err == nil {
		t.Fatalf("exhausted check retained old task score %v", score)
	}
	if _, err := rdb.ZScore(ctx, reclaimCheckScheduleKey("rt-a"), "future-task").Result(); err != nil {
		t.Fatalf("exhausted check removed concurrent/future task: %v", err)
	}
}
