package exec

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"

	"github.com/muandane/grizzle/internal/plan"
)

// BackfillFunc defines the hook function signature for batch backfilling columns outside the DDL lock window.
type BackfillFunc func(ctx context.Context, tx *sql.Tx, table, oldCol, newCol string) error

// RunBackfill executes the backfill hook outside the DDL lock window in batches.
func RunBackfill(ctx context.Context, db *sql.DB, targetSchema string, renames map[string]string, steps []plan.Step, hook BackfillFunc, logger *slog.Logger) error {
	if hook == nil || len(renames) == 0 || db == nil {
		return nil
	}

	for key, newCol := range renames {
		table, oldCol := parseRenameTarget(key)
		if table == "" {
			// Find table from steps if not qualified in the key
			for _, s := range steps {
				if s.Table != "" && strings.Contains(s.SQL, newCol) {
					table = s.Table
					break
				}
			}
		}
		if table == "" || oldCol == "" || newCol == "" {
			continue
		}

		tblRef := table
		if targetSchema != "" && targetSchema != "main" && targetSchema != "public" {
			tblRef = fmt.Sprintf("%q.%q", targetSchema, table)
		} else {
			tblRef = fmt.Sprintf("%q", table)
		}

		// Check if newCol exists and how many rows have newCol IS NULL
		checkQuery := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %q IS NULL;", tblRef, newCol)
		var nullCount int
		if err := db.QueryRowContext(ctx, checkQuery).Scan(&nullCount); err != nil {
			// If column or table does not exist or error querying, skip
			continue
		}

		if logger != nil {
			logger.DebugContext(ctx, "grizzle: running backfill hook", "table", table, "old_column", oldCol, "new_column", newCol, "null_count", nullCount)
		}

		prevCount := nullCount
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}

			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf("grizzle: backfill begin tx failed: %w", err)
			}

			if err := hook(ctx, tx, table, oldCol, newCol); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("grizzle: backfill hook failed for table %s (%s -> %s): %w", table, oldCol, newCol, err)
			}

			if err := tx.Commit(); err != nil {
				return fmt.Errorf("grizzle: backfill commit failed for table %s: %w", table, err)
			}

			if nullCount == 0 {
				break
			}

			var curCount int
			if err := db.QueryRowContext(ctx, checkQuery).Scan(&curCount); err != nil {
				break
			}

			if curCount == 0 || curCount >= prevCount {
				break
			}
			prevCount = curCount
		}
	}
	return nil
}

func parseRenameTarget(key string) (table, oldCol string) {
	if dot := strings.Index(key, "."); dot != -1 {
		return key[:dot], key[dot+1:]
	}
	return "", key
}
