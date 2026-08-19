// Package issueactivitybackfill provides bounded, resumable batches for
// seeding issue.last_activity_at from the pre-existing updated_at history.
package issueactivitybackfill

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultBatchSize bounds each transaction and the number of issue rows it
// locks. Operators can add a delay between calls from the command wrapper.
const DefaultBatchSize = 1000

// Options controls one backfill batch. Zero values are production defaults.
type Options struct {
	BatchSize int
	// Table is a trusted identifier override used only by tests.
	Table string
}

// Batch copies updated_at into last_activity_at for at most BatchSize rows.
// Rows already touched by a current writer no longer match the outer predicate,
// so a concurrent semantic update always wins over historical reconstruction.
func Batch(ctx context.Context, pool *pgxpool.Pool, opts Options) (int64, error) {
	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = DefaultBatchSize
	}
	table := opts.Table
	if table == "" {
		table = "issue"
	}

	query := fmt.Sprintf(`
WITH batch AS (
    SELECT id
    FROM %s
    WHERE last_activity_at IS NULL
    ORDER BY id
    LIMIT $1
    FOR UPDATE SKIP LOCKED
)
UPDATE %s i
SET last_activity_at = i.updated_at
FROM batch
WHERE i.id = batch.id
  AND i.last_activity_at IS NULL`, table, table)
	tag, err := pool.Exec(ctx, query, batchSize)
	if err != nil {
		return 0, fmt.Errorf("backfill issue last_activity_at batch: %w", err)
	}
	return tag.RowsAffected(), nil
}

// CountRemaining reports rows not yet reconstructed.
func CountRemaining(ctx context.Context, pool *pgxpool.Pool, table string) (int64, error) {
	if table == "" {
		table = "issue"
	}
	var count int64
	if err := pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT count(*) FROM %s WHERE last_activity_at IS NULL", table,
	)).Scan(&count); err != nil {
		return 0, fmt.Errorf("count issue last_activity_at backlog: %w", err)
	}
	return count, nil
}
