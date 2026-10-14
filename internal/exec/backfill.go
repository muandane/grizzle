package exec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/muandane/grizzle/internal/plan"
)

// Dialect identifies the database dialect for backfill operations.
type Dialect string

// Dialect constants for PostgreSQL and SQLite.
const (
	DialectPostgres Dialect = "postgres"
	DialectSQLite   Dialect = "sqlite"
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

		var tblRef string
		if strings.Contains(table, ".") {
			tblRef = quoteTableRef(table)
		} else if targetSchema != "" && targetSchema != "main" && targetSchema != "public" {
			tblRef = fmt.Sprintf("%q.%q", targetSchema, table)
		} else {
			tblRef = fmt.Sprintf("%q", table)
		}

		// Check if newCol exists and how many rows have newCol IS NULL
		//nolint:gosec // G201: tblRef and newCol are structurally validated from internal schema definitions
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
	if dot := strings.LastIndex(key, "."); dot != -1 {
		return key[:dot], key[dot+1:]
	}
	return "", key
}

// NewCopyBackfillHook builds a BackfillFunc that copies values from oldCol to newCol
// using keyset pagination ordered by primary key. It supports N-column composite primary
// keys (N >= 1) using SQL row-value comparisons: (pk1, ..., pkN) > ($1, ..., $N).
//
// NULL-free assumption:
// Primary key columns are guaranteed NOT NULL by relational database constraints (SQL
// standard, PostgreSQL, and SQLite). Because keyset pagination compares
// (col1, ..., colN) > (cursor1, ..., cursorN), all operand values are non-null. This
// eliminates SQL three-valued logic (NULL / unknown) edge cases in row-value comparisons,
// ensuring strictly two-valued (true or false) evaluations and deterministic progression.
//
// The hook requires and verifies that a database index exists whose leading columns match
// the primary key columns in exact order before keyset traversal begins.
func NewCopyBackfillHook(d Dialect, batch int) BackfillFunc {
	pkCache := map[string][]string{}
	lastPK := map[string][]any{}
	started := map[string]bool{}

	return func(ctx context.Context, tx *sql.Tx, table, oldCol, newCol string) error {
		pkCols, ok := pkCache[table]
		if !ok {
			var err error
			schemaName, tableName := splitSchemaAndTable(table)
			pkCols, err = LookupTablePK(ctx, tx, d, schemaName, tableName)
			if err != nil {
				return err
			}
			pkCache[table] = pkCols
		}

		tableRef := quoteTableRef(table)
		hasCursor := started[table]
		lastCursor := lastPK[table]

		selectSQL := BuildKeysetSelectSQL(d, tableRef, pkCols, newCol, hasCursor)
		var args []any
		if hasCursor {
			args = append(args, lastCursor...)
		}
		args = append(args, batch)

		rows, err := tx.QueryContext(ctx, selectSQL, args...)
		if err != nil {
			return fmt.Errorf("copy backfill select: %w", err)
		}
		defer func() { _ = rows.Close() }()

		var batchRows [][]any
		scanPtrs := make([]any, len(pkCols))
		for rows.Next() {
			rowVals := make([]any, len(pkCols))
			for i := range rowVals {
				scanPtrs[i] = &rowVals[i]
			}
			if err := rows.Scan(scanPtrs...); err != nil {
				return fmt.Errorf("copy backfill scan: %w", err)
			}
			// Normalize any byte slices to strings if needed
			for i, v := range rowVals {
				if b, ok := v.([]byte); ok {
					rowVals[i] = string(b)
				}
			}
			batchRows = append(batchRows, rowVals)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("copy backfill scan rows: %w", err)
		}
		if len(batchRows) == 0 {
			return nil
		}

		updateSQL := BuildKeysetUpdateSQL(d, tableRef, pkCols, oldCol, newCol, len(batchRows))
		updateArgs := make([]any, 0, len(batchRows)*len(pkCols))
		for _, row := range batchRows {
			updateArgs = append(updateArgs, row...)
		}

		if _, err := tx.ExecContext(ctx, updateSQL, updateArgs...); err != nil {
			return fmt.Errorf("copy backfill update: %w", err)
		}

		started[table] = true
		lastPK[table] = batchRows[len(batchRows)-1]
		return nil
	}
}

// LookupTablePK returns the primary key columns in constraint order and verifies that
// an index exists on table whose leading columns match the PK order.
func LookupTablePK(ctx context.Context, tx *sql.Tx, d Dialect, targetSchema, table string) ([]string, error) {
	switch d {
	case DialectPostgres:
		return lookupPostgresPK(ctx, tx, targetSchema, table)
	default:
		return lookupSQLitePK(ctx, tx, targetSchema, table)
	}
}

func lookupPostgresPK(ctx context.Context, tx *sql.Tx, targetSchema, table string) ([]string, error) {
	const pkQ = `
SELECT COALESCE(string_agg(a.attname, ',' ORDER BY u.pos), '') AS pk_columns
FROM pg_constraint con
JOIN pg_class c ON c.oid = con.conrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
CROSS JOIN LATERAL unnest(con.conkey) WITH ORDINALITY AS u(attnum, pos)
JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = u.attnum
WHERE con.contype = 'p'
  AND c.relname = $1
  AND (n.nspname = $2 OR ($2 = '' AND n.nspname = current_schema()))
GROUP BY con.conname;`

	var pkColsJoined string
	err := tx.QueryRowContext(ctx, pkQ, table, targetSchema).Scan(&pkColsJoined)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("copy backfill requires a primary key on %q (found 0 PK columns)", table)
		}
		return nil, fmt.Errorf("looking up primary key on %q: %w", table, err)
	}
	if strings.TrimSpace(pkColsJoined) == "" {
		return nil, fmt.Errorf("copy backfill requires a primary key on %q (found 0 PK columns)", table)
	}
	pkCols := strings.Split(pkColsJoined, ",")

	const idxQ = `
SELECT i.relname AS index_name,
       COALESCE(string_agg(a.attname, ',' ORDER BY u.pos), '') AS index_columns
FROM pg_index idx
JOIN pg_class c ON c.oid = idx.indrelid
JOIN pg_class i ON i.oid = idx.indexrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
CROSS JOIN LATERAL unnest(idx.indkey) WITH ORDINALITY AS u(attnum, pos)
JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = u.attnum
WHERE c.relname = $1
  AND (n.nspname = $2 OR ($2 = '' AND n.nspname = current_schema()))
  AND idx.indisvalid
GROUP BY i.relname;`

	rows, err := tx.QueryContext(ctx, idxQ, table, targetSchema)
	if err != nil {
		return nil, fmt.Errorf("verifying index on %q: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	var hasMatching bool
	for rows.Next() {
		var idxName, idxColsJoined string
		if err := rows.Scan(&idxName, &idxColsJoined); err != nil {
			return nil, fmt.Errorf("scanning index on %q: %w", table, err)
		}
		idxCols := strings.Split(idxColsJoined, ",")
		if matchesLeading(idxCols, pkCols) {
			hasMatching = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !hasMatching {
		return nil, fmt.Errorf("copy backfill requires an index matching primary key column order (%s) on %q", strings.Join(pkCols, ", "), table)
	}
	return pkCols, nil
}

func lookupSQLitePK(ctx context.Context, tx *sql.Tx, targetSchema, table string) ([]string, error) {
	tableInfoQ := fmt.Sprintf(`PRAGMA table_info(%q)`, table)
	if targetSchema != "" && targetSchema != "main" {
		tableInfoQ = fmt.Sprintf(`PRAGMA %q.table_info(%q)`, targetSchema, table)
	}

	rows, err := tx.QueryContext(ctx, tableInfoQ)
	if err != nil {
		return nil, fmt.Errorf("looking up table info for %q: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	type pkEntry struct {
		name string
		pk   int
		typ  string
	}
	var pks []pkEntry
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, fmt.Errorf("scanning table_info for %q: %w", table, err)
		}
		if pk > 0 {
			pks = append(pks, pkEntry{name: name, pk: pk, typ: strings.ToUpper(typ)})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(pks) == 0 {
		return nil, fmt.Errorf("copy backfill requires a primary key on %q (found 0 PK columns)", table)
	}

	sort.Slice(pks, func(i, j int) bool {
		return pks[i].pk < pks[j].pk
	})

	pkCols := make([]string, len(pks))
	for i, p := range pks {
		pkCols[i] = p.name
	}

	idxListQ := fmt.Sprintf(`PRAGMA index_list(%q)`, table)
	if targetSchema != "" && targetSchema != "main" {
		idxListQ = fmt.Sprintf(`PRAGMA %q.index_list(%q)`, targetSchema, table)
	}

	idxRows, err := tx.QueryContext(ctx, idxListQ)
	if err != nil {
		return nil, fmt.Errorf("querying index_list for %q: %w", table, err)
	}
	defer func() { _ = idxRows.Close() }()

	var indexNames []string
	for idxRows.Next() {
		var seq, unique, partial int
		var name, origin string
		if err := idxRows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			return nil, fmt.Errorf("scanning index_list for %q: %w", table, err)
		}
		indexNames = append(indexNames, name)
	}
	if err := idxRows.Err(); err != nil {
		return nil, err
	}

	var hasMatching bool
	for _, idxName := range indexNames {
		idxInfoQ := fmt.Sprintf(`PRAGMA index_info(%q)`, idxName)
		if targetSchema != "" && targetSchema != "main" {
			idxInfoQ = fmt.Sprintf(`PRAGMA %q.index_info(%q)`, targetSchema, idxName)
		}
		infoRows, err := tx.QueryContext(ctx, idxInfoQ)
		if err != nil {
			continue
		}
		var cols []string
		for infoRows.Next() {
			var seqno, cid int
			var cname string
			if err := infoRows.Scan(&seqno, &cid, &cname); err == nil {
				cols = append(cols, cname)
			}
		}
		_ = infoRows.Close()
		if matchesLeading(cols, pkCols) {
			hasMatching = true
			break
		}
	}

	if !hasMatching {
		// Single integer column PK acts as the rowid B-Tree index in SQLite
		if len(pkCols) == 1 && strings.Contains(pks[0].typ, "INT") {
			hasMatching = true
		}
	}

	if !hasMatching {
		return nil, fmt.Errorf("copy backfill requires an index matching primary key column order (%s) on %q", strings.Join(pkCols, ", "), table)
	}
	return pkCols, nil
}

func matchesLeading(idxCols, pkCols []string) bool {
	if len(idxCols) < len(pkCols) {
		return false
	}
	for i := range pkCols {
		if idxCols[i] != pkCols[i] {
			return false
		}
	}
	return true
}

// BuildKeysetSelectSQL builds the keyset SELECT statement for selecting PK batches.
func BuildKeysetSelectSQL(d Dialect, tableRef string, pkCols []string, newCol string, hasCursor bool) string {
	quotedCols := make([]string, len(pkCols))
	orderList := make([]string, len(pkCols))
	for i, c := range pkCols {
		quotedCols[i] = fmt.Sprintf("%q", c)
		orderList[i] = fmt.Sprintf("%q ASC", c)
	}
	colsJoined := strings.Join(quotedCols, ", ")
	orderByJoined := strings.Join(orderList, ", ")

	if !hasCursor {
		if d == DialectPostgres {
			return fmt.Sprintf(`SELECT %s FROM %s WHERE %q IS NULL ORDER BY %s LIMIT $1`,
				colsJoined, tableRef, newCol, orderByJoined)
		}
		return fmt.Sprintf(`SELECT %s FROM %s WHERE %q IS NULL ORDER BY %s LIMIT ?`,
			colsJoined, tableRef, newCol, orderByJoined)
	}

	if len(pkCols) == 1 {
		if d == DialectPostgres {
			return fmt.Sprintf(`SELECT %s FROM %s WHERE %q IS NULL AND %s > $1 ORDER BY %s LIMIT $2`,
				colsJoined, tableRef, newCol, quotedCols[0], orderByJoined)
		}
		return fmt.Sprintf(`SELECT %s FROM %s WHERE %q IS NULL AND %s > ? ORDER BY %s LIMIT ?`,
			colsJoined, tableRef, newCol, quotedCols[0], orderByJoined)
	}

	// N >= 2 composite keys: row-value compare (col1, col2, ...) > ($1, $2, ...)
	if d == DialectPostgres {
		placeholders := make([]string, len(pkCols))
		for i := range pkCols {
			placeholders[i] = fmt.Sprintf("$%d", i+1)
		}
		limitPlaceholder := fmt.Sprintf("$%d", len(pkCols)+1)
		return fmt.Sprintf(`SELECT %s FROM %s WHERE %q IS NULL AND (%s) > (%s) ORDER BY %s LIMIT %s`,
			colsJoined, tableRef, newCol, colsJoined, strings.Join(placeholders, ", "), orderByJoined, limitPlaceholder)
	}

	placeholders := make([]string, len(pkCols))
	for i := range pkCols {
		placeholders[i] = "?"
	}
	return fmt.Sprintf(`SELECT %s FROM %s WHERE %q IS NULL AND (%s) > (%s) ORDER BY %s LIMIT ?`,
		colsJoined, tableRef, newCol, colsJoined, strings.Join(placeholders, ", "), orderByJoined)
}

// BuildKeysetUpdateSQL builds the batch UPDATE statement matching the selected PK tuples.
func BuildKeysetUpdateSQL(d Dialect, tableRef string, pkCols []string, oldCol, newCol string, numRows int) string {
	if numRows <= 0 {
		return ""
	}

	if len(pkCols) == 1 {
		var b strings.Builder
		b.Grow(len(tableRef) + len(newCol) + len(oldCol) + len(pkCols[0]) + 40 + numRows*6)
		b.WriteString("UPDATE ")
		b.WriteString(tableRef)
		b.WriteString(" SET ")
		fmt.Fprintf(&b, "%q = %q", newCol, oldCol)
		b.WriteString(" WHERE ")
		fmt.Fprintf(&b, "%q", pkCols[0])
		b.WriteString(" IN (")
		for i := range numRows {
			if i > 0 {
				b.WriteString(", ")
			}
			if d == DialectPostgres {
				b.WriteString("$")
				b.WriteString(strconv.Itoa(i + 1))
			} else {
				b.WriteString("?")
			}
		}
		b.WriteString(")")
		return b.String()
	}

	// Composite primary key (N >= 2): WHERE (col1, col2) IN ((?, ?), (?, ?), ...)
	var b strings.Builder
	b.Grow(len(tableRef) + len(newCol) + len(oldCol) + 64 + numRows*(len(pkCols)*6+4))
	b.WriteString("UPDATE ")
	b.WriteString(tableRef)
	b.WriteString(" SET ")
	fmt.Fprintf(&b, "%q = %q", newCol, oldCol)
	b.WriteString(" WHERE (")
	for i, c := range pkCols {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q", c)
	}
	b.WriteString(") IN (")
	paramIdx := 1
	for r := range numRows {
		if r > 0 {
			b.WriteString(", (")
		} else {
			b.WriteString("(")
		}
		for c := range pkCols {
			if c > 0 {
				b.WriteString(", ")
			}
			if d == DialectPostgres {
				b.WriteString("$")
				b.WriteString(strconv.Itoa(paramIdx))
				paramIdx++
			} else {
				b.WriteString("?")
			}
		}
		b.WriteString(")")
	}
	b.WriteString(")")
	return b.String()
}

func quoteTableRef(table string) string {
	if schema, tbl, ok := strings.Cut(table, "."); ok {
		return fmt.Sprintf("%q.%q", schema, tbl)
	}
	return fmt.Sprintf("%q", table)
}

func splitSchemaAndTable(table string) (schema, tbl string) {
	if s, t, ok := strings.Cut(table, "."); ok {
		return s, t
	}
	return "", table
}
