package exec

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"time"
)

// releaseLockTimeout bounds the advisory-lock release and session-state reset
// performed after a migration attempt finishes (successfully or not).
const releaseLockTimeout = 5 * time.Second

// releasePostgresConn releases the acquired advisory locks and resets session
// state on the dedicated migration connection.
//
// The release runs on a context detached from the migration context
// (context.WithoutCancel) and bounded by a timeout, so it still executes when
// the caller cancelled ctx mid-migration and cannot hang forever on a wedged
// connection.
//
// If releasing or resetting fails, the connection is discarded by returning
// driver.ErrBadConn from conn.Raw: database/sql then closes the connection
// instead of pooling it, so a session that may still hold advisory locks (or
// leaked session state) is never handed back to other callers.
func releasePostgresConn(ctx context.Context, conn *sql.Conn, release func(context.Context) error) {
	relCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseLockTimeout)
	defer cancel()

	err := release(relCtx)
	if err == nil {
		// Reset session state; on failure the session leaks search_path and
		// timeout settings back into the pool, so discard as well.
		if _, resetErr := conn.ExecContext(relCtx, "RESET search_path; RESET lock_timeout; RESET statement_timeout;"); resetErr != nil {
			err = resetErr
		}
	}
	if err != nil {
		_ = conn.Raw(func(_ any) error { return driver.ErrBadConn })
	}
}
