package postgres

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"slices"
	"time"

	"github.com/muandane/grizzle/internal/backoff"
	"github.com/muandane/grizzle/internal/dialect"
	"github.com/muandane/grizzle/internal/plan"
)

// lockPollMaxBackoff caps the advisory-lock polling backoff well below the retry
// campaign ceiling: migrators must grab the lock promptly once it is released,
// while still cutting poll QPS ~10x versus the fixed 20ms ticker.
const lockPollMaxBackoff = 250 * time.Millisecond

// GenerateLockID produces a deterministic 64-bit integer hash from a schema identifier.
func GenerateLockID(schema string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("grizzle:schema_lock:" + schema))
	return int64(h.Sum64())
}

// VerifySessionDedicated probes whether the connection preserves session state
// across queries. Connection poolers operating in transaction pooling mode (such
// as PgBouncer with pool_mode = transaction) reassign or reset (DISCARD ALL) the
// underlying server connection between statements, which breaks session-level
// advisory locks and leads to orphaned locks or hangs.
func VerifySessionDedicated(ctx context.Context, dbtx dialect.DBTX) error {
	const probeVal = "grizzle_session_active"
	// Set custom GUC in session scope (is_local = false)
	var setDummy string
	err := dbtx.QueryRowContext(ctx, "SELECT set_config('grizzle.session_probe', $1, false);", probeVal).Scan(&setDummy)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return nil
	}

	var gotVal string
	err = dbtx.QueryRowContext(ctx, "SELECT current_setting('grizzle.session_probe', true);").Scan(&gotVal)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return nil
	}

	// Clean up probe setting on best effort
	var clearDummy string
	_ = dbtx.QueryRowContext(ctx, "SELECT set_config('grizzle.session_probe', '', false);").Scan(&clearDummy)

	if gotVal != probeVal {
		return fmt.Errorf("%w: session state was not preserved across queries (expected %q, got %q); connect directly to PostgreSQL or configure PgBouncer in session pooling mode", plan.ErrTransactionPooling, probeVal, gotVal)
	}
	return nil
}

// AcquireSessionAdvisoryLock acquires a PostgreSQL session-level exclusive advisory lock on a dedicated connection.
// It uses pg_try_advisory_lock in a non-blocking loop to avoid holding open server-side lock wait queues
// that would cause PostgreSQL deadlock detection against concurrent non-transactional DDL such as CREATE INDEX CONCURRENTLY.
func AcquireSessionAdvisoryLock(ctx context.Context, dbtx dialect.DBTX, lockID int64) error {
	if err := VerifySessionDedicated(ctx, dbtx); err != nil {
		return err
	}

	attempt := 0
	for {
		var acquired bool
		err := dbtx.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1);", lockID).Scan(&acquired)
		if err != nil {
			// Context expiry while polling must surface as ErrLockTimeout so
			// Sync/Apply retry logic (IsRetryable) can back off and retry.
			if ctx.Err() != nil {
				return fmt.Errorf("%w: %w", plan.ErrLockTimeout, err)
			}
			return fmt.Errorf("failed checking pg_try_advisory_lock: %w", err)
		}
		if acquired {
			return nil
		}

		timer := time.NewTimer(backoff.ComputeWithMax(attempt, nil, lockPollMaxBackoff))
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("%w: %v", plan.ErrLockTimeout, ctx.Err())
		case <-timer.C:
		}
		attempt++
	}
}

// ReleaseSessionAdvisoryLock releases a PostgreSQL session-level exclusive advisory lock on a dedicated connection.
func ReleaseSessionAdvisoryLock(ctx context.Context, dbtx dialect.DBTX, lockID int64) error {
	var released bool
	err := dbtx.QueryRowContext(ctx, "SELECT pg_advisory_unlock($1);", lockID).Scan(&released)
	if err != nil {
		return fmt.Errorf("failed to release pg_advisory_unlock: %w", err)
	}
	if !released {
		return fmt.Errorf("%w: advisory lock was not held by this connection (possible transaction-mode pooling or connection reuse)", plan.ErrTransactionPooling)
	}
	return nil
}

// Hash32 computes a deterministic 32-bit signed integer hash from a string.
func Hash32(s string) int32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return int32(h.Sum32())
}

// AcquireSessionAdvisoryLock2 acquires a PostgreSQL session-level advisory lock using two 32-bit keys (namespace, key).
func AcquireSessionAdvisoryLock2(ctx context.Context, dbtx dialect.DBTX, key1, key2 int32) error {
	if err := VerifySessionDedicated(ctx, dbtx); err != nil {
		return err
	}

	attempt := 0
	for {
		var acquired bool
		err := dbtx.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1, $2);", key1, key2).Scan(&acquired)
		if err != nil {
			// Context expiry while polling must surface as ErrLockTimeout so
			// Sync/Apply retry logic (IsRetryable) can back off and retry.
			if ctx.Err() != nil {
				return fmt.Errorf("%w: %w", plan.ErrLockTimeout, err)
			}
			return fmt.Errorf("failed checking pg_try_advisory_lock($1, $2): %w", err)
		}
		if acquired {
			return nil
		}

		timer := time.NewTimer(backoff.ComputeWithMax(attempt, nil, lockPollMaxBackoff))
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("%w: %v", plan.ErrLockTimeout, ctx.Err())
		case <-timer.C:
		}
		attempt++
	}
}

// ReleaseSessionAdvisoryLock2 releases a PostgreSQL session-level advisory lock using two 32-bit keys (namespace, key).
func ReleaseSessionAdvisoryLock2(ctx context.Context, dbtx dialect.DBTX, key1, key2 int32) error {
	var released bool
	err := dbtx.QueryRowContext(ctx, "SELECT pg_advisory_unlock($1, $2);", key1, key2).Scan(&released)
	if err != nil {
		return fmt.Errorf("failed to release pg_advisory_unlock($1, $2): %w", err)
	}
	if !released {
		return fmt.Errorf("%w: advisory lock was not held by this connection (possible transaction-mode pooling or connection reuse)", plan.ErrTransactionPooling)
	}
	return nil
}

// AcquireSchemaLocks acquires session-level advisory locks for the given schemas in sorted, deduped order
// using the two-int form (hash32(namespace), hash32(schema)).
// If any lock acquisition fails, all locks acquired so far are released in reverse order before returning the error.
func AcquireSchemaLocks(ctx context.Context, dbtx dialect.DBTX, namespace string, schemas []string) ([]string, error) {
	if namespace == "" {
		namespace = "grizzle"
	}
	nsKey := Hash32(namespace)

	sorted := make([]string, len(schemas))
	copy(sorted, schemas)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)

	acquired := make([]string, 0, len(sorted))
	for _, s := range sorted {
		schemaKey := Hash32(s)
		if err := AcquireSessionAdvisoryLock2(ctx, dbtx, nsKey, schemaKey); err != nil {
			// Roll back all acquired locks in reverse order
			for _, a := range slices.Backward(acquired) {
				_ = ReleaseSessionAdvisoryLock2(context.Background(), dbtx, nsKey, Hash32(a))
			}
			return nil, err
		}
		acquired = append(acquired, s)
	}
	return acquired, nil
}

// ReleaseSchemaLocks releases session-level advisory locks for the given schemas in reverse order.
func ReleaseSchemaLocks(ctx context.Context, dbtx dialect.DBTX, namespace string, schemas []string) error {
	if namespace == "" {
		namespace = "grizzle"
	}
	nsKey := Hash32(namespace)

	var firstErr error
	for _, schema := range slices.Backward(schemas) {
		schemaKey := Hash32(schema)
		if err := ReleaseSessionAdvisoryLock2(ctx, dbtx, nsKey, schemaKey); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// SchemaTable identifies a table within a specific PostgreSQL schema.
type SchemaTable struct {
	Schema string
	Table  string
}

// QueryTableLockContention queries pg_locks and pg_stat_activity for concurrent sessions
// holding granted locks on the target schema.table, excluding the current backend.
//
// NOTE: Pre-flight lock inspection is inherently racy: concurrent transactions may acquire
// locks between the pre-flight check and subsequent DDL execution.
func QueryTableLockContention(ctx context.Context, dbtx dialect.DBTX, schema, table string) ([]plan.LockConflict, error) {
	if dbtx == nil {
		return nil, fmt.Errorf("grizzle: database connection is nil")
	}
	const q = `
SELECT a.pid,
       COALESCE(a.usename, ''),
       COALESCE(a.application_name, ''),
       COALESCE(a.state, ''),
       COALESCE(a.query, ''),
       COALESCE(EXTRACT(EPOCH FROM (clock_timestamp() - a.query_start)), 0)::float8 AS duration_seconds,
       l.mode
FROM pg_locks l
JOIN pg_stat_activity a ON l.pid = a.pid
JOIN pg_class c ON l.relation = c.oid
JOIN pg_namespace n ON c.relnamespace = n.oid
WHERE n.nspname = $1 AND c.relname = $2
  AND a.pid != pg_backend_pid()
  AND l.granted = true
ORDER BY a.pid ASC;`

	rows, err := dbtx.QueryContext(ctx, q, schema, table)
	if err != nil {
		return nil, fmt.Errorf("querying lock contention on %s.%s: %w", schema, table, err)
	}
	defer func() { _ = rows.Close() }()

	var conflicts []plan.LockConflict
	for rows.Next() {
		var c plan.LockConflict
		c.Schema = schema
		c.Table = table
		var durSec float64
		if err := rows.Scan(&c.PID, &c.Username, &c.ApplicationName, &c.State, &c.Query, &durSec, &c.LockMode); err != nil {
			return nil, fmt.Errorf("scanning lock conflict: %w", err)
		}
		c.Duration = time.Duration(durSec * float64(time.Second))
		conflicts = append(conflicts, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating lock conflicts on %s.%s: %w", schema, table, err)
	}
	return conflicts, nil
}

// PreflightLockContention inspects pg_locks and pg_stat_activity on target tables before DDL execution.
// If failOnContention is false (advisory mode), warnings are logged for any conflicting holders and execution proceeds.
// If failOnContention is true (gating mode), backoff retries occur up to timeout; if contention persists,
// an error wrapping plan.ErrLockContention is returned.
//
// NOTE: Pre-flight lock inspection is inherently racy: concurrent transactions may acquire
// locks between the pre-flight check and subsequent DDL execution. Always configure lock_timeout
// alongside fail_on_contention to bound wait duration if a race occurs.
func PreflightLockContention(ctx context.Context, dbtx dialect.DBTX, tables []SchemaTable, timeout time.Duration, failOnContention bool, logger *slog.Logger) error {
	if len(tables) == 0 {
		return nil
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	checkAll := func() ([]plan.LockConflict, error) {
		var all []plan.LockConflict
		for _, st := range tables {
			conflicts, err := QueryTableLockContention(ctx, dbtx, st.Schema, st.Table)
			if err != nil {
				return nil, err
			}
			all = append(all, conflicts...)
		}
		return all, nil
	}

	// Advisory mode: single check, log warnings for any conflicts, do not abort
	if !failOnContention {
		conflicts, err := checkAll()
		if err != nil {
			if logger != nil {
				logger.WarnContext(ctx, "grizzle: pre-flight lock contention check failed", "error", err)
			}
			return nil
		}
		for _, c := range conflicts {
			if logger != nil {
				logger.WarnContext(ctx, "grizzle: lock contention detected on target table (advisory)",
					"schema", c.Schema,
					"table", c.Table,
					"pid", c.PID,
					"user", c.Username,
					"mode", c.LockMode,
					"duration", c.Duration,
					"query", c.Query,
				)
			}
		}
		return nil
	}

	// Gating mode: retry with backoff up to timeout
	deadline := time.Now().Add(timeout)
	attempt := 0
	for {
		conflicts, err := checkAll()
		if err != nil {
			return err
		}
		if len(conflicts) == 0 {
			if attempt > 0 && logger != nil {
				logger.InfoContext(ctx, "grizzle: lock contention cleared on target tables")
			}
			return nil
		}

		if time.Now().After(deadline) || ctx.Err() != nil {
			if logger != nil {
				logger.ErrorContext(ctx, "grizzle: migration gated by lock contention on target table",
					"conflicts_count", len(conflicts),
					"first_pid", conflicts[0].PID,
					"first_table", conflicts[0].Table,
				)
			}
			return &plan.LockContentionError{Conflicts: conflicts}
		}

		b := backoff.ComputeWithMax(attempt, nil, 250*time.Millisecond)
		remaining := time.Until(deadline)
		if b > remaining {
			b = remaining
		}

		if logger != nil {
			logger.WarnContext(ctx, "grizzle: lock contention detected on target table, retrying with backoff",
				"attempt", attempt,
				"backoff_ms", b.Milliseconds(),
				"conflicts_count", len(conflicts),
			)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(b):
		}
		attempt++
	}
}
