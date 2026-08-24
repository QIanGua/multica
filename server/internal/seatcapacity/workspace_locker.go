package seatcapacity

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const workspaceLockReleaseTimeout = 3 * time.Second
const defaultWorkspaceCooldown = 75 * time.Millisecond

type WorkspaceLocker interface {
	Lock(context.Context, uuid.UUID) (db.DBTX, func(), error)
}

// postgresWorkspaceLocker serializes Cloud capacity calls for one workspace
// across API replicas. It pins a PostgreSQL session because advisory locks are
// session-scoped; returning the connection before unlock would leak the lock
// into an unrelated pool borrower.
type postgresWorkspaceLocker struct {
	pool *pgxpool.Pool
}

func NewWorkspaceLocker(pool *pgxpool.Pool) WorkspaceLocker {
	if pool == nil {
		return nil
	}
	return &postgresWorkspaceLocker{pool: pool}
}

func (l *postgresWorkspaceLocker) Lock(ctx context.Context, workspaceID uuid.UUID) (db.DBTX, func(), error) {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("acquire workspace capacity lock connection: %w", err)
	}
	key := workspaceAdvisoryLockKey(workspaceID)
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		conn.Release()
		return nil, nil, fmt.Errorf("acquire workspace capacity lock: %w", err)
	}
	return conn, func() {
		timer := time.NewTimer(defaultWorkspaceCooldown)
		<-timer.C
		unlockCtx, cancel := context.WithTimeout(context.Background(), workspaceLockReleaseTimeout)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, key); err != nil {
			// A session-level lock must never return to the pool if explicit
			// unlock failed. Closing the hijacked connection releases it in
			// PostgreSQL and keeps the next pool borrower isolated.
			_ = conn.Hijack().Close(unlockCtx)
			return
		}
		conn.Release()
	}, nil
}

func workspaceAdvisoryLockKey(workspaceID uuid.UUID) int64 {
	sum := sha256.Sum256(append([]byte("multica-seat-capacity:"), workspaceID[:]...))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}
