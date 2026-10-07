package exec

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/muandane/grizzle/internal/dialect"
)

// readSQLiteForeignKeys reads the current foreign_keys enforcement setting on
// the given connection. The PRAGMA is per-connection state in SQLite.
func readSQLiteForeignKeys(ctx context.Context, dbtx dialect.DBTX) (bool, error) {
	var on int
	if err := dbtx.QueryRowContext(ctx, "PRAGMA foreign_keys;").Scan(&on); err != nil {
		return false, fmt.Errorf("sqlite: failed reading PRAGMA foreign_keys: %w", err)
	}
	return on != 0, nil
}

// setSQLiteForeignKeys writes the foreign_keys setting and reads it back,
// aborting if the driver did not apply it (silently keeping FK enforcement on
// during a table rebuild can cascade-delete child rows).
func setSQLiteForeignKeys(ctx context.Context, dbtx dialect.DBTX, on bool) error {
	value := 0
	if on {
		value = 1
	}
	if _, err := dbtx.ExecContext(ctx, fmt.Sprintf("PRAGMA foreign_keys = %d;", value)); err != nil {
		return fmt.Errorf("sqlite: failed setting PRAGMA foreign_keys = %d: %w", value, err)
	}
	now, err := readSQLiteForeignKeys(ctx, dbtx)
	if err != nil {
		return err
	}
	if now != on {
		return fmt.Errorf("sqlite: PRAGMA foreign_keys = %d did not take effect (reads back as %t)", value, now)
	}
	return nil
}

// runSQLiteWithForeignKeysOff pins a single pooled connection, turns foreign
// key enforcement off on it, runs fn inside a transaction bound to that same
// connection, and restores the caller's previous foreign_keys setting before
// the connection returns to the pool.
//
// PRAGMA foreign_keys is per-connection state and a no-op inside a
// transaction, so the pragma, the transaction, and the restore must all run
// on the same pinned *sql.Conn. fn also receives the pinned conn so failure
// bookkeeping (e.g. history records) lands on the same database the
// migration ran against; with ":memory:" every pooled connection is a
// separate database, so writing via *sql.DB can target the wrong one.
func runSQLiteWithForeignKeysOff(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx, conn *sql.Conn) error) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("sqlite: failed to acquire connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	prev, err := readSQLiteForeignKeys(ctx, conn)
	if err != nil {
		return err
	}

	if err := setSQLiteForeignKeys(ctx, conn, false); err != nil {
		return err
	}

	err = func() error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("sqlite: failed to begin transaction: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		if err := fn(tx, conn); err != nil {
			return err
		}

		if err := tx.Commit(); err != nil {
			return err
		}
		return nil
	}()

	// Restore the caller's previous value (not a hardcoded ON): if foreign
	// keys were disabled by the caller's DSN, we must not silently enable
	// them. Use a detached context: restoring must happen even when ctx was
	// cancelled mid-transaction. setSQLiteForeignKeys reads the value back,
	// so a driver that fails to apply the restore is reported.
	if rErr := setSQLiteForeignKeys(context.WithoutCancel(ctx), conn, prev); rErr != nil && err == nil {
		err = rErr
	}
	return err
}
