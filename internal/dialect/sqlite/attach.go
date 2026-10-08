package sqlite

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/muandane/grizzle/internal/dialect"
)

// AttachDatabases runs ATTACH DATABASE for each schema→path entry on the
// pinned connection. Names are validated upstream; paths are bound as
// parameters. "main" must not appear — it is the already-open primary DB.
func AttachDatabases(ctx context.Context, dbtx dialect.DBTX, attach map[string]string) error {
	if len(attach) == 0 {
		return nil
	}
	names := slices.Sorted(maps.Keys(attach))
	for _, name := range names {
		path := attach[name]
		// Schema name is a validated identifier; path is parameterized.
		stmt := fmt.Sprintf(`ATTACH DATABASE ? AS %q`, name)
		if _, err := dbtx.ExecContext(ctx, stmt, path); err != nil {
			_ = DetachDatabases(ctx, dbtx, names[:slices.Index(names, name)])
			return fmt.Errorf("sqlite: ATTACH DATABASE %q AS %q: %w", path, name, err)
		}
	}
	return nil
}

// DetachDatabases runs DETACH DATABASE for each name (reverse order).
func DetachDatabases(ctx context.Context, dbtx dialect.DBTX, names []string) error {
	var first error
	for _, name := range slices.Backward(names) {

		if _, err := dbtx.ExecContext(ctx, fmt.Sprintf(`DETACH DATABASE %q`, name)); err != nil && first == nil {
			first = fmt.Errorf("sqlite: DETACH DATABASE %q: %w", name, err)
		}
	}
	return first
}

// AttachMemorySchemas attaches empty in-memory databases for each schema name
// other than "main". Used by shadow compilation so desired DDL can target
// attached schemas without touching live files.
func AttachMemorySchemas(ctx context.Context, dbtx dialect.DBTX, schemas []string) error {
	var attached []string
	for _, name := range schemas {
		if name == "" || name == "main" {
			continue
		}
		stmt := fmt.Sprintf(`ATTACH DATABASE ':memory:' AS %q`, name)
		if _, err := dbtx.ExecContext(ctx, stmt); err != nil {
			_ = DetachDatabases(ctx, dbtx, attached)
			return fmt.Errorf("sqlite: ATTACH ':memory:' AS %q: %w", name, err)
		}
		attached = append(attached, name)
	}
	return nil
}
