package exec

import (
	"context"
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
