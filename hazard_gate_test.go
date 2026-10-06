package grizzle_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/muandane/grizzle"
	"github.com/muandane/grizzle/internal/plan"
	_ "modernc.org/sqlite"
)

func TestHazardGate_DirectUnit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		plan          *plan.Plan
		accept        []plan.HazardCode
		expectBlocked bool
		expectedCode  plan.HazardCode
	}{
		{
			name: "DROP_TABLE blocked by default",
			plan: &plan.Plan{
				Steps: []plan.Step{
					{Type: plan.ChangeDropTable, Table: "users", SQL: "DROP TABLE users;", Destructive: true},
				},
			},
			accept:        nil,
			expectBlocked: true,
			expectedCode:  plan.HazardDropTable,
		},
		{
			name: "DROP_TABLE unblocked by matching code",
			plan: &plan.Plan{
				Steps: []plan.Step{
					{Type: plan.ChangeDropTable, Table: "users", SQL: "DROP TABLE users;", Destructive: true},
				},
			},
			accept:        []plan.HazardCode{plan.HazardDropTable},
			expectBlocked: false,
		},
		{
			name: "DROP_TABLE blocked by wrong code",
			plan: &plan.Plan{
				Steps: []plan.Step{
					{Type: plan.ChangeDropTable, Table: "users", SQL: "DROP TABLE users;", Destructive: true},
				},
			},
			accept:        []plan.HazardCode{plan.HazardDropColumn},
			expectBlocked: true,
			expectedCode:  plan.HazardDropTable,
		},
		{
			name: "DROP_COLUMN blocked by default",
			plan: &plan.Plan{
				Steps: []plan.Step{
					{Type: plan.ChangeDropColumn, Table: "users", SQL: "ALTER TABLE users DROP COLUMN email;", Destructive: true},
				},
			},
			accept:        nil,
			expectBlocked: true,
			expectedCode:  plan.HazardDropColumn,
		},
		{
			name: "DROP_COLUMN unblocked by matching code",
			plan: &plan.Plan{
				Steps: []plan.Step{
					{Type: plan.ChangeDropColumn, Table: "users", SQL: "ALTER TABLE users DROP COLUMN email;", Destructive: true},
				},
			},
			accept:        []plan.HazardCode{plan.HazardDropColumn},
			expectBlocked: false,
		},
		{
			name: "TYPE_NARROW blocked by default",
			plan: &plan.Plan{
				Steps: []plan.Step{
					{Type: plan.ChangeAlterColumn, Table: "users", SQL: "ALTER TABLE users ALTER COLUMN age TYPE smallint;", Destructive: true, TypeNarrowed: true},
				},
			},
			accept:        nil,
			expectBlocked: true,
			expectedCode:  plan.HazardTypeNarrow,
		},
		{
			name: "TYPE_NARROW unblocked by matching code",
			plan: &plan.Plan{
				Steps: []plan.Step{
					{Type: plan.ChangeAlterColumn, Table: "users", SQL: "ALTER TABLE users ALTER COLUMN age TYPE smallint;", Destructive: true, TypeNarrowed: true},
				},
			},
			accept:        []plan.HazardCode{plan.HazardTypeNarrow},
			expectBlocked: false,
		},
		{
			name: "NOT_NULL_NO_DEFAULT blocked by default",
			plan: &plan.Plan{
				Steps: []plan.Step{
					{Type: plan.ChangeAddColumn, Table: "users", SQL: "ALTER TABLE users ADD COLUMN age int NOT NULL;", ColumnNotNull: true, ColumnHasDefault: false},
				},
			},
			accept:        nil,
			expectBlocked: true,
			expectedCode:  plan.HazardNotNullNoDefault,
		},
		{
			name: "NOT_NULL_NO_DEFAULT unblocked by matching code",
			plan: &plan.Plan{
				Steps: []plan.Step{
					{Type: plan.ChangeAddColumn, Table: "users", SQL: "ALTER TABLE users ADD COLUMN age int NOT NULL;", ColumnNotNull: true, ColumnHasDefault: false},
				},
			},
			accept:        []plan.HazardCode{plan.HazardNotNullNoDefault},
			expectBlocked: false,
		},
		{
			name: "NOTICE hazards do not block",
			plan: &plan.Plan{
				Steps: []plan.Step{
					{Type: plan.ChangeCreateIndex, Table: "users", SQL: "CREATE INDEX idx_users_name ON users(name);"},
					{Type: plan.ChangeDropIndex, Table: "users", SQL: "DROP INDEX idx_old;"},
					{Type: plan.ChangeDropFK, Table: "users", SQL: "ALTER TABLE users DROP CONSTRAINT fk_old;"},
				},
			},
			accept:        nil,
			expectBlocked: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.plan.ValidateHazards(tt.accept)
			if tt.expectBlocked {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if !errors.Is(err, grizzle.ErrHazardBlocked) {
					t.Errorf("expected ErrHazardBlocked sentinel, got: %v", err)
				}
				var herr *grizzle.HazardError
				if !errors.As(err, &herr) {
					t.Fatalf("expected *HazardError typed error, got %T", err)
				}
				if len(herr.Hazards) == 0 || herr.Hazards[0].Code != tt.expectedCode {
					t.Errorf("expected hazard code %s, got: %+v", tt.expectedCode, herr.Hazards)
				}
			} else {
				if err != nil {
					t.Fatalf("expected no error, got: %v", err)
				}
			}
		})
	}
}

func setupSQLiteDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestHazardGate_SQLiteEndToEnd(t *testing.T) {
	ctx := context.Background()

	t.Run("DROP_TABLE: blocked by default, unblocked by accept, hard-blocked by AllowDrop=false", func(t *testing.T) {
		db := setupSQLiteDB(t)
		initial := "CREATE TABLE users (id INTEGER PRIMARY KEY); CREATE TABLE drop_me (id INTEGER PRIMARY KEY);"
		if err := grizzle.Sync(ctx, db, grizzle.Options{SchemaSQL: initial}); err != nil {
			t.Fatalf("initial sync failed: %v", err)
		}

		desired := "CREATE TABLE users (id INTEGER PRIMARY KEY);"

		// 1. AllowDrop=true but no AcceptHazards -> ErrHazardBlocked
		err := grizzle.Sync(ctx, db, grizzle.Options{
			SchemaSQL: desired,
			AllowDrop: true,
		})
		if !errors.Is(err, grizzle.ErrHazardBlocked) {
			t.Fatalf("expected ErrHazardBlocked, got: %v", err)
		}

		// 2. AllowDrop=false with AcceptHazards -> hard-blocked by AllowDrop (ErrDestructiveBlocked)
		err = grizzle.Sync(ctx, db, grizzle.Options{
			SchemaSQL:     desired,
			AllowDrop:     false,
			AcceptHazards: []grizzle.HazardCode{grizzle.HazardDropTable},
		})
		if !errors.Is(err, grizzle.ErrDestructiveBlocked) {
			t.Fatalf("expected ErrDestructiveBlocked, got: %v", err)
		}

		// 3. AllowDrop=true with wrong hazard code -> ErrHazardBlocked
		err = grizzle.Sync(ctx, db, grizzle.Options{
			SchemaSQL:     desired,
			AllowDrop:     true,
			AcceptHazards: []grizzle.HazardCode{grizzle.HazardDropColumn},
		})
		if !errors.Is(err, grizzle.ErrHazardBlocked) {
			t.Fatalf("expected ErrHazardBlocked with wrong code, got: %v", err)
		}

		// 4. AllowDrop=true with correct AcceptHazards -> SUCCESS
		err = grizzle.Sync(ctx, db, grizzle.Options{
			SchemaSQL:     desired,
			AllowDrop:     true,
			AcceptHazards: []grizzle.HazardCode{grizzle.HazardDropTable},
		})
		if err != nil {
			t.Fatalf("expected sync to succeed with AcceptHazards, got: %v", err)
		}
	})

	t.Run("DROP_COLUMN: blocked by default, unblocked by accept, hard-blocked by AllowDrop=false", func(t *testing.T) {
		db := setupSQLiteDB(t)
		initial := "CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT, status TEXT);"
		if err := grizzle.Sync(ctx, db, grizzle.Options{SchemaSQL: initial}); err != nil {
			t.Fatalf("initial sync failed: %v", err)
		}

		desired := "CREATE TABLE users (id INTEGER PRIMARY KEY, status TEXT);"

		// 1. AllowDrop=true without AcceptHazards -> ErrHazardBlocked
		err := grizzle.Sync(ctx, db, grizzle.Options{
			SchemaSQL: desired,
			AllowDrop: true,
		})
		if !errors.Is(err, grizzle.ErrHazardBlocked) {
			t.Fatalf("expected ErrHazardBlocked, got: %v", err)
		}

		// 2. AllowDrop=false with AcceptHazards -> hard-blocked by AllowDrop (ErrDestructiveBlocked)
		err = grizzle.Sync(ctx, db, grizzle.Options{
			SchemaSQL:     desired,
			AllowDrop:     false,
			AcceptHazards: []grizzle.HazardCode{grizzle.HazardDropColumn},
		})
		if !errors.Is(err, grizzle.ErrDestructiveBlocked) {
			t.Fatalf("expected ErrDestructiveBlocked, got: %v", err)
		}

		// 3. AllowDrop=true with matching AcceptHazards -> SUCCESS
		err = grizzle.Sync(ctx, db, grizzle.Options{
			SchemaSQL:     desired,
			AllowDrop:     true,
			AcceptHazards: []grizzle.HazardCode{grizzle.HazardDropColumn},
		})
		if err != nil {
			t.Fatalf("expected sync to succeed, got: %v", err)
		}
	})

	t.Run("NOT_NULL_NO_DEFAULT: blocked by default, unblocked by accept", func(t *testing.T) {
		db := setupSQLiteDB(t)
		initial := "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT);"
		if err := grizzle.Sync(ctx, db, grizzle.Options{SchemaSQL: initial}); err != nil {
			t.Fatalf("initial sync failed: %v", err)
		}

		desired := "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT, email TEXT NOT NULL);"

		// 1. Without AcceptHazards -> ErrHazardBlocked
		err := grizzle.Sync(ctx, db, grizzle.Options{
			SchemaSQL: desired,
		})
		if !errors.Is(err, grizzle.ErrHazardBlocked) {
			t.Fatalf("expected ErrHazardBlocked, got: %v", err)
		}

		// 2. With wrong code -> ErrHazardBlocked
		err = grizzle.Sync(ctx, db, grizzle.Options{
			SchemaSQL:     desired,
			AcceptHazards: []grizzle.HazardCode{grizzle.HazardDropTable},
		})
		if !errors.Is(err, grizzle.ErrHazardBlocked) {
			t.Fatalf("expected ErrHazardBlocked with wrong code, got: %v", err)
		}

		// 3. With matching code -> SUCCESS
		err = grizzle.Sync(ctx, db, grizzle.Options{
			SchemaSQL:     desired,
			AcceptHazards: []grizzle.HazardCode{grizzle.HazardNotNullNoDefault},
		})
		if err != nil {
			t.Fatalf("expected sync to succeed with HazardNotNullNoDefault, got: %v", err)
		}
	})
}
