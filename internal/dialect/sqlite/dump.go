package sqlite

import (
	"context"
	"fmt"
	"strings"

	"github.com/muandane/grizzle/internal/dialect"
)

// DumpSchema extracts the canonical DDL SQL for all user objects from the SQLite catalog.
func DumpSchema(ctx context.Context, dbtx dialect.DBTX, schemaName string) (string, error) {
	if schemaName == "" {
		schemaName = "main"
	}
	catalog := fmt.Sprintf("%q.sqlite_schema", schemaName)
	query := fmt.Sprintf(`
		SELECT type, name, sql
		FROM %s
		WHERE name NOT LIKE 'sqlite_%%'
		  AND name NOT LIKE '_grizzle_%%'
		  AND name != 'grizzle_history'
		  AND sql IS NOT NULL
		ORDER BY
			CASE type
				WHEN 'table' THEN 1
				WHEN 'view' THEN 2
				WHEN 'index' THEN 3
				WHEN 'trigger' THEN 4
				ELSE 5
			END,
			name;
	`, catalog)

	rows, err := dbtx.QueryContext(ctx, query)
	if err != nil {
		return "", fmt.Errorf("sqlite dump: querying %s: %w", catalog, err)
	}
	defer func() { _ = rows.Close() }()

	var stmts []string
	for rows.Next() {
		var typ, name, rawSQL string
		if err := rows.Scan(&typ, &name, &rawSQL); err != nil {
			return "", fmt.Errorf("sqlite dump: scanning row: %w", err)
		}
		trimmed := strings.TrimSpace(rawSQL)
		if trimmed == "" {
			continue
		}
		if !strings.HasSuffix(trimmed, ";") {
			trimmed += ";"
		}
		stmts = append(stmts, trimmed)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("sqlite dump: iterating rows: %w", err)
	}

	if len(stmts) == 0 {
		return "", nil
	}
	return strings.Join(stmts, "\n\n") + "\n", nil
}
