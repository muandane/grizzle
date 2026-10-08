package exec

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/muandane/grizzle/internal/dialect"
	"github.com/muandane/grizzle/internal/dialect/sqlite"
	"github.com/muandane/grizzle/internal/plan"
)

// withSQLiteAttachedConn pins a pooled connection, ATTACHes configured
// databases, runs fn, then DETACHes. When attach is empty, fn still runs on a
// pinned connection (required for :memory: consistency) without ATTACH.
func withSQLiteAttachedConn(ctx context.Context, db *sql.DB, attach map[string]string, fn func(conn *sql.Conn) error) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("sqlite: failed to acquire connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	names := slices.Sorted(maps.Keys(attach))
	if err := sqlite.AttachDatabases(ctx, conn, attach); err != nil {
		return err
	}
	defer func() { _ = sqlite.DetachDatabases(context.WithoutCancel(ctx), conn, names) }()

	return fn(conn)
}

// runSQLiteConnWithForeignKeysOff is like runSQLiteWithForeignKeysOff but uses
// an already-pinned connection (so ATTACH state remains visible).
func runSQLiteConnWithForeignKeysOff(ctx context.Context, conn *sql.Conn, fn func(tx *sql.Tx) error) error {
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

		if err := fn(tx); err != nil {
			return err
		}
		return tx.Commit()
	}()

	if rErr := setSQLiteForeignKeys(context.WithoutCancel(ctx), conn, prev); rErr != nil && err == nil {
		err = rErr
	}
	return err
}

// sqliteForeignKeyCheck runs PRAGMA [schema.]foreign_key_check for each
// managed schema. Cross-database FKs are not enforced by SQLite and are not
// validated here.
func sqliteForeignKeyCheck(ctx context.Context, tx *sql.Tx, schemas []string) error {
	if len(schemas) == 0 {
		schemas = []string{"main"}
	}
	var fkViolations []string
	for _, sch := range schemas {
		var q string
		if sch == "" || sch == "main" {
			q = "PRAGMA foreign_key_check;"
		} else {
			q = fmt.Sprintf("PRAGMA %q.foreign_key_check;", sch)
		}
		rows, err := tx.QueryContext(ctx, q)
		if err != nil {
			return fmt.Errorf("sqlite: foreign key check failed for schema %q: %w", sch, err)
		}
		for rows.Next() {
			var vTbl, vParent string
			var vRowID, vFKID int64
			if err := rows.Scan(&vTbl, &vRowID, &vParent, &vFKID); err == nil {
				fkViolations = append(fkViolations, fmt.Sprintf("schema %q table %q row %d -> %q", sch, vTbl, vRowID, vParent))
			}
		}
		_ = rows.Close()
	}
	if len(fkViolations) > 0 {
		return fmt.Errorf("sqlite: foreign key constraint violation: %s", joinSemicolon(fkViolations))
	}
	return nil
}

func joinSemicolon(parts []string) string {
	return strings.Join(parts, "; ")
}

// buildSQLitePlan compiles desired schemas in an isolated shadow, inspects live
// schemas from dbtx (which must already have ATTACHes applied when needed),
// and diffs per schema.
func buildSQLitePlan(ctx context.Context, dbtx dialect.DBTX, cfg SQLiteExecConfig) (*plan.Plan, error) {
	schemas := cfg.targetSchemas()
	desiredMap, err := sqlite.CompileInShadowSchemas(ctx, cfg.SchemaSQL, schemas)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", plan.ErrCompilationFailed, err)
	}
	liveMap, err := sqlite.InspectSchemas(ctx, dbtx, schemas)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", plan.ErrInspectionFailed, err)
	}

	var steps []plan.Step
	for _, sch := range schemas {
		steps = append(steps, sqlite.Diff(liveMap[sch], desiredMap[sch], cfg.Filters)...)
	}

	p := &plan.Plan{
		TargetSchema:   cfg.primarySchema(),
		Steps:          steps,
		Policy:         cfg.Policy,
		IncludeTables:  cfg.Filters.Includes,
		ExcludeTables:  cfg.Filters.Excludes,
		Renames:        cfg.Filters.Renames,
		ExpandContract: cfg.Filters.ExpandContract,
		SchemaSQL:      cfg.SchemaSQL,
	}
	// Preserve single-schema plan hash stability: omit TargetSchemas when
	// managing only the primary database (historical SQLite plans used nil).
	if len(schemas) > 1 {
		p.TargetSchemas = schemas
	}
	return p, nil
}
