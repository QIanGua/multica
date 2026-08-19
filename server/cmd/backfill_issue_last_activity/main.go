// backfill_issue_last_activity reconstructs issue.last_activity_at from the
// existing updated_at value. Run it after every issue-writing backend in a
// rolling deployment has been upgraded; older writers do not maintain the new
// column. The command is deliberately separate from migrations and startup so
// a large issue table never blocks a deploy.
//
// Each batch commits independently. SIGINT/SIGTERM, a database error, or
// --max-batches can stop the walk, and a later invocation resumes from rows
// whose last_activity_at is still NULL. A session advisory lock serializes
// operators while SKIP LOCKED avoids waiting behind unrelated hot rows.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/issueactivitybackfill"
	"github.com/multica-ai/multica/server/internal/logger"
)

const advisoryLockName = "issue_last_activity_backfill"

func main() {
	logger.Init()
	if err := run(); err != nil {
		slog.Error("issue last-activity backfill failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	batchSize := flag.Int("batch-size", issueactivitybackfill.DefaultBatchSize, "maximum issue rows updated per transaction")
	delay := flag.Duration("sleep-between-batches", 100*time.Millisecond, "delay between committed batches")
	maxBatches := flag.Int("max-batches", 0, "stop after N batches (0 = finish all remaining rows)")
	flag.Parse()
	if *batchSize < 1 {
		return fmt.Errorf("--batch-size must be at least 1")
	}
	if *delay < 0 {
		return fmt.Errorf("--sleep-between-batches must not be negative")
	}
	if *maxBatches < 0 {
		return fmt.Errorf("--max-batches must not be negative")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	lockConn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire advisory-lock connection: %w", err)
	}
	defer lockConn.Release()
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, advisoryLockName); err != nil {
		return fmt.Errorf("acquire advisory lock: %w", err)
	}
	defer func() {
		_, _ = lockConn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, advisoryLockName)
	}()

	remaining, err := issueactivitybackfill.CountRemaining(ctx, pool, "")
	if err != nil {
		return err
	}
	slog.Info("issue last-activity backfill started", "remaining", remaining, "batch_size", *batchSize, "delay", delay.String())

	var total int64
	for batch := 1; *maxBatches == 0 || batch <= *maxBatches; batch++ {
		rows, err := issueactivitybackfill.Batch(ctx, pool, issueactivitybackfill.Options{BatchSize: *batchSize})
		if err != nil {
			return err
		}
		total += rows
		if rows > 0 {
			slog.Info("issue last-activity batch committed", "batch", batch, "rows", rows, "total", total)
		}
		// A short/empty SKIP LOCKED batch does not prove completion: all
		// remaining rows may simply be hot. Count before declaring success and
		// keep retrying until they unlock or the operator interrupts the run.
		if rows < int64(*batchSize) {
			remaining, err = issueactivitybackfill.CountRemaining(ctx, pool, "")
			if err != nil {
				return err
			}
			if remaining == 0 {
				slog.Info("issue last-activity backfill complete", "rows_backfilled", total, "remaining", 0)
				return nil
			}
			slog.Info("issue last-activity rows remain locked or pending", "remaining", remaining)
		}
		if *delay > 0 {
			select {
			case <-time.After(*delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}

	remaining, err = issueactivitybackfill.CountRemaining(ctx, pool, "")
	if err != nil {
		return err
	}
	slog.Info("issue last-activity backfill stopped at max batches", "rows_backfilled", total, "remaining", remaining)
	return nil
}
