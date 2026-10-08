package grizzle_test

import (
	"errors"
	"testing"

	"github.com/muandane/grizzle"
)

func TestOptions_Validate_SQLiteAttach(t *testing.T) {
	base := grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: "CREATE TABLE t (id INTEGER);",
	}

	t.Run("requires attach path for non-main target", func(t *testing.T) {
		o := base
		o.TargetSchemas = []string{"main", "aux"}
		err := o.Validate()
		if !errors.Is(err, grizzle.ErrInvalidOptions) {
			t.Fatalf("expected ErrInvalidOptions, got %v", err)
		}
	})

	t.Run("rejects main in SQLiteAttach", func(t *testing.T) {
		o := base
		o.TargetSchemas = []string{"main"}
		o.SQLiteAttach = map[string]string{"main": "/tmp/x.db"}
		err := o.Validate()
		if !errors.Is(err, grizzle.ErrInvalidOptions) {
			t.Fatalf("expected ErrInvalidOptions, got %v", err)
		}
	})

	t.Run("rejects empty path", func(t *testing.T) {
		o := base
		o.TargetSchemas = []string{"main", "aux"}
		o.SQLiteAttach = map[string]string{"aux": "  "}
		err := o.Validate()
		if !errors.Is(err, grizzle.ErrInvalidOptions) {
			t.Fatalf("expected ErrInvalidOptions, got %v", err)
		}
	})

	t.Run("rejects unknown attach key", func(t *testing.T) {
		o := base
		o.TargetSchemas = []string{"main", "aux"}
		o.SQLiteAttach = map[string]string{"aux": "/tmp/a.db", "other": "/tmp/b.db"}
		err := o.Validate()
		if !errors.Is(err, grizzle.ErrInvalidOptions) {
			t.Fatalf("expected ErrInvalidOptions, got %v", err)
		}
	})

	t.Run("rejects duplicate TargetSchemas", func(t *testing.T) {
		o := base
		o.TargetSchemas = []string{"main", "aux", "aux"}
		o.SQLiteAttach = map[string]string{"aux": "/tmp/a.db"}
		err := o.Validate()
		if !errors.Is(err, grizzle.ErrInvalidOptions) {
			t.Fatalf("expected ErrInvalidOptions, got %v", err)
		}
	})

	t.Run("accepts valid multi-schema attach", func(t *testing.T) {
		o := base
		o.TargetSchemas = []string{"main", "aux"}
		o.SQLiteAttach = map[string]string{"aux": "/tmp/a.db"}
		if err := o.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("no longer returns ErrUnsupportedMultiSchema", func(t *testing.T) {
		o := base
		o.TargetSchemas = []string{"main", "aux"}
		o.SQLiteAttach = map[string]string{"aux": "/tmp/a.db"}
		err := o.Validate()
		if errors.Is(err, grizzle.ErrUnsupportedMultiSchema) {
			t.Fatal("SQLite multi-schema must not return ErrUnsupportedMultiSchema")
		}
		if err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})
}
