package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/muandane/grizzle"
)

// runLint statically lints a schema.sql file. SQLite compilation is fully
// offline (in-memory shadow); PostgreSQL compilation uses the DSN shadow path.
func runLint(ctx context.Context, dsn, schemaFile, format string, failOnWarning bool) int {
	content, err := os.ReadFile(filepath.Clean(schemaFile)) //nolint:gosec // G304: CLI accepts user-provided schema file path
	if err != nil {
		slog.Error("reading schema file", "path", schemaFile, "err", err)
		return 2
	}

	var db *sql.DB
	if dsn == "" {
		// Offline default: compile in an in-memory SQLite shadow database.
		db, err = sql.Open("sqlite", ":memory:")
		if err != nil {
			slog.Error("opening in-memory database", "err", err)
			return 2
		}
	} else {
		db, err = initDB(dsn)
		if err != nil {
			return 1
		}
	}
	defer func() { _ = db.Close() }()

	opts := grizzle.Options{
		SchemaSQL: string(content),
		Dialect:   grizzle.DialectAuto,
	}
	ir, err := grizzle.CompileSchema(ctx, db, opts)
	if err != nil {
		slog.Error("compiling schema", "path", schemaFile, "err", err)
		return 2
	}

	diags := grizzle.LintSchema(ir)

	switch format {
	case "json":
		if err := grizzle.LintFormatJSON(os.Stdout, diags); err != nil {
			slog.Error("formatting lint output", "err", err)
			return 1
		}
	case "github":
		if err := grizzle.LintFormatGitHub(os.Stdout, diags); err != nil {
			slog.Error("formatting lint output", "err", err)
			return 1
		}
	default:
		if err := grizzle.LintFormatText(os.Stdout, diags); err != nil {
			slog.Error("formatting lint output", "err", err)
			return 1
		}
	}

	if grizzle.LintHasErrors(diags) {
		fmt.Fprintf(os.Stderr, "Lint failed with %d error(s)\n", countLintSeverity(diags, grizzle.LintSeverityError))
		return 1
	}
	if failOnWarning {
		if n := countLintSeverity(diags, grizzle.LintSeverityWarning); n > 0 {
			fmt.Fprintf(os.Stderr, "Lint failed with %d warning(s) (--fail-on-warning)\n", n)
			return 1
		}
	}
	if len(diags) == 0 {
		fmt.Println("Lint passed: no diagnostics.")
	}
	return 0
}

func countLintSeverity(diags []grizzle.LintDiagnostic, sev grizzle.LintSeverity) int {
	n := 0
	for _, d := range diags {
		if d.Severity == sev {
			n++
		}
	}
	return n
}
