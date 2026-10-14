package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/muandane/grizzle"
	"github.com/muandane/grizzle/internal/exec"
)

// backfillFlag is a custom flag.Value for --backfill with an optional argument.
// Bare --backfill (via IsBoolFlag) defaults to the "copy" strategy; --backfill=copy
// and (after normalizeBackfillArgs) --backfill copy are equivalent.
type backfillFlag struct {
	set      bool
	strategy string
}

func (f *backfillFlag) String() string {
	if !f.set {
		return ""
	}
	return f.strategy
}

func (f *backfillFlag) Set(v string) error {
	f.set = true
	switch v {
	case "", "true": // bare --backfill via IsBoolFlag
		f.strategy = "copy"
	default:
		f.strategy = v
	}
	return nil
}

// IsBoolFlag lets --backfill appear with no value (defaults to copy).
func (f *backfillFlag) IsBoolFlag() bool { return true }

// normalizeBackfillArgs rewrites `--backfill <strategy>` into `--backfill=<strategy>`
// so the space-separated form works alongside IsBoolFlag bare `--backfill`.
func normalizeBackfillArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--backfill" {
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				out = append(out, "--backfill="+args[i+1])
				i++
			} else {
				out = append(out, "--backfill=copy")
			}
			continue
		}
		out = append(out, a)
	}
	return out
}

func dialectFromDSN(dsn string) grizzle.Dialect {
	switch {
	case strings.HasPrefix(dsn, "sqlite://"),
		strings.HasPrefix(dsn, "sqlite:"),
		strings.HasSuffix(dsn, ".db"),
		strings.HasSuffix(dsn, ".sqlite"),
		dsn == ":memory:":
		return grizzle.DialectSQLite
	default:
		return grizzle.DialectPostgres
	}
}

// resolveBackfillHook builds a BackfillFunc from CLI flags, or nil when disabled.
func resolveBackfillHook(expandContract bool, bf backfillFlag, file string, batch int, dsn string) (grizzle.BackfillFunc, error) {
	if !bf.set && file == "" {
		return nil, nil
	}
	if !expandContract {
		return nil, fmt.Errorf("--backfill/--backfill-file require --expand-contract")
	}
	if bf.set && file != "" {
		return nil, fmt.Errorf("cannot combine --backfill and --backfill-file; choose one")
	}
	if batch <= 0 {
		return nil, fmt.Errorf("--backfill-batch must be positive, got %d", batch)
	}
	if file != "" {
		content, err := os.ReadFile(filepath.Clean(file)) //nolint:gosec // G304: CLI accepts user-provided SQL file path
		if err != nil {
			return nil, fmt.Errorf("reading --backfill-file: %w", err)
		}
		return newFileBackfillHook(string(content), batch), nil
	}
	if bf.strategy != "copy" {
		return nil, fmt.Errorf("unsupported --backfill strategy %q (supported: copy)", bf.strategy)
	}
	return newCopyBackfillHook(dialectFromDSN(dsn), batch), nil
}

func newCopyBackfillHook(d grizzle.Dialect, batch int) grizzle.BackfillFunc {
	execDialect := exec.DialectPostgres
	if d == grizzle.DialectSQLite {
		execDialect = exec.DialectSQLite
	}
	return grizzle.BackfillFunc(exec.NewCopyBackfillHook(execDialect, batch))
}

func newFileBackfillHook(templateSQL string, batch int) grizzle.BackfillFunc {
	return func(ctx context.Context, tx *sql.Tx, table, oldCol, newCol string) error {
		stmt := applyBackfillTemplate(templateSQL, table, oldCol, newCol, batch)
		//nolint:gosec // G701: user-supplied --backfill-file SQL; identifiers substituted via %q
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("file backfill exec: %w", err)
		}
		return nil
	}
}

func applyBackfillTemplate(tmpl, table, oldCol, newCol string, batch int) string {
	r := strings.NewReplacer(
		"{table}", fmt.Sprintf("%q", table),
		"{old}", fmt.Sprintf("%q", oldCol),
		"{new}", fmt.Sprintf("%q", newCol),
		"{batch}", strconv.Itoa(batch),
	)
	return r.Replace(tmpl)
}
