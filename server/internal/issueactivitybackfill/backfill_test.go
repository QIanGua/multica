package issueactivitybackfill

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Skipf("connect to database: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Skipf("database not reachable: %v", err)
	}
	return pool
}

func fixture(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	schema := fmt.Sprintf("issue_activity_backfill_%d_%d", time.Now().UnixNano(), rand.IntN(1_000_000))
	if _, err := pool.Exec(context.Background(), fmt.Sprintf(`CREATE SCHEMA %q`, schema)); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP SCHEMA %q CASCADE`, schema))
	})
	table := fmt.Sprintf("%q.issue", schema)
	if _, err := pool.Exec(context.Background(), fmt.Sprintf(`
CREATE TABLE %s (
    id UUID PRIMARY KEY,
    updated_at TIMESTAMPTZ NOT NULL,
    last_activity_at TIMESTAMPTZ
)`, table)); err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	return table
}

func TestBatchIsBoundedAndResumable(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	ctx := context.Background()
	table := fixture(t, pool)
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s (id, updated_at, last_activity_at) VALUES
('00000000-0000-0000-0000-000000000001', '2026-01-01T00:00:00Z', NULL),
('00000000-0000-0000-0000-000000000002', '2026-01-02T00:00:00Z', NULL),
('00000000-0000-0000-0000-000000000003', '2026-01-03T00:00:00Z', '2026-02-01T00:00:00Z')`, table)); err != nil {
		t.Fatalf("seed fixture: %v", err)
	}

	rows, err := Batch(ctx, pool, Options{BatchSize: 1, Table: table})
	if err != nil || rows != 1 {
		t.Fatalf("first Batch = (%d, %v), want (1, nil)", rows, err)
	}
	remaining, err := CountRemaining(ctx, pool, table)
	if err != nil || remaining != 1 {
		t.Fatalf("CountRemaining = (%d, %v), want (1, nil)", remaining, err)
	}
	rows, err = Batch(ctx, pool, Options{BatchSize: 10, Table: table})
	if err != nil || rows != 1 {
		t.Fatalf("second Batch = (%d, %v), want (1, nil)", rows, err)
	}
	rows, err = Batch(ctx, pool, Options{BatchSize: 10, Table: table})
	if err != nil || rows != 0 {
		t.Fatalf("idempotent Batch = (%d, %v), want (0, nil)", rows, err)
	}

	var preserved time.Time
	if err := pool.QueryRow(ctx, fmt.Sprintf(`
SELECT last_activity_at FROM %s WHERE id = '00000000-0000-0000-0000-000000000003'`, table)).Scan(&preserved); err != nil {
		t.Fatalf("read pre-populated row: %v", err)
	}
	if want := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC); !preserved.Equal(want) {
		t.Fatalf("pre-populated activity changed to %s, want %s", preserved, want)
	}
}
