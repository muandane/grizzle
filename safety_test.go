package grizzle

import (
	"bytes"
	"strings"
	"testing"
)

func TestSafety_ExcludeTables_Preserved(t *testing.T) {
	live := &SchemaIR{
		Name: "public",
		Tables: map[string]*TableIR{
			"users": {
				Name:    "users",
				Columns: map[string]*ColumnIR{"id": {Name: "id", DataType: "bigint", Position: 1}},
			},
			"spatial_ref_sys": {
				Name:    "spatial_ref_sys",
				Columns: map[string]*ColumnIR{"srid": {Name: "srid", DataType: "integer", Position: 1}},
			},
			"asynq_tasks": {
				Name:    "asynq_tasks",
				Columns: map[string]*ColumnIR{"id": {Name: "id", DataType: "text", Position: 1}},
			},
			"temporal_executions": {
				Name:    "temporal_executions",
				Columns: map[string]*ColumnIR{"run_id": {Name: "run_id", DataType: "text", Position: 1}},
			},
			"unmanaged_legacy": {
				Name:    "unmanaged_legacy",
				Columns: map[string]*ColumnIR{"id": {Name: "id", DataType: "int", Position: 1}},
			},
			"[unclosed_bracket_literal": {
				Name:    "[unclosed_bracket_literal",
				Columns: map[string]*ColumnIR{"id": {Name: "id", DataType: "int", Position: 1}},
			},
		},
		Enums: make(map[string]*EnumIR),
	}

	desired := &SchemaIR{
		Name: "_grizzle_shadow",
		Tables: map[string]*TableIR{
			"users": {
				Name:    "users",
				Columns: map[string]*ColumnIR{"id": {Name: "id", DataType: "bigint", Position: 1}},
			},
		},
		Enums: make(map[string]*EnumIR),
	}

	excludes := []string{"asynq_*", "temporal_*", "[unclosed_bracket_literal"}
	filters := tableFilters{excludes: excludes}
	steps := diffSchemas(live, desired, "public", "_grizzle_shadow", filters)

	// spatial_ref_sys is in built-in ignore list -> never dropped
	// asynq_tasks matches asynq_* -> never dropped
	// temporal_executions matches temporal_* -> never dropped
	// [unclosed_bracket_literal has invalid glob syntax, but literal fallback protects it -> never dropped
	// unmanaged_legacy is NOT excluded -> should be dropped
	if len(steps) != 1 {
		t.Fatalf("expected exactly 1 step (dropping unmanaged_legacy), got %d steps: %+v", len(steps), steps)
	}

	if steps[0].Type != ChangeDropTable || steps[0].Table != "unmanaged_legacy" {
		t.Errorf("expected DROP TABLE unmanaged_legacy, got step: %+v", steps[0])
	}

	// Verify SQLite also respects filters
	sqliteSteps := diffSQLiteSchemas(live, desired, filters)
	if len(sqliteSteps) != 1 {
		t.Fatalf("expected exactly 1 SQLite step, got %d steps: %+v", len(sqliteSteps), sqliteSteps)
	}
	if sqliteSteps[0].Type != ChangeDropTable || sqliteSteps[0].Table != "unmanaged_legacy" {
		t.Errorf("expected SQLite DROP TABLE unmanaged_legacy, got step: %+v", sqliteSteps[0])
	}
}

func TestSafety_IncludeTables_Whitelist(t *testing.T) {
	live := &SchemaIR{
		Name: "public",
		Tables: map[string]*TableIR{
			"users": {
				Name:    "users",
				Columns: map[string]*ColumnIR{"id": {Name: "id", DataType: "bigint", Position: 1}},
			},
			"accounts": {
				Name:    "accounts",
				Columns: map[string]*ColumnIR{"id": {Name: "id", DataType: "bigint", Position: 1}},
			},
			"audit_logs": {
				Name:    "audit_logs",
				Columns: map[string]*ColumnIR{"id": {Name: "id", DataType: "bigint", Position: 1}},
			},
		},
		Enums: make(map[string]*EnumIR),
	}

	desired := &SchemaIR{
		Name: "_grizzle_shadow",
		Tables: map[string]*TableIR{
			"users": {
				Name: "users",
				Columns: map[string]*ColumnIR{
					"id":   {Name: "id", DataType: "bigint", Position: 1},
					"name": {Name: "name", DataType: "text", Position: 2},
				},
			},
			"accounts": {
				Name: "accounts",
				Columns: map[string]*ColumnIR{
					"id":   {Name: "id", DataType: "bigint", Position: 1},
					"iban": {Name: "iban", DataType: "text", Position: 2},
				},
			},
		},
		Enums: make(map[string]*EnumIR),
	}

	filters := tableFilters{includes: []string{"users"}}
	steps := diffSchemas(live, desired, "public", "_grizzle_shadow", filters)

	// Only "users" table should be altered (adding "name" column).
	// "accounts" is omitted because it is not in IncludeTables.
	// "audit_logs" is omitted because it is not in IncludeTables, so it is never dropped.
	if len(steps) != 1 {
		t.Fatalf("expected exactly 1 step for included table users, got %d steps: %+v", len(steps), steps)
	}

	if steps[0].Type != ChangeAddColumn || steps[0].Table != "users" {
		t.Errorf("expected ChangeAddColumn on users, got: %+v", steps[0])
	}
	if !steps[0].ColumnNotNull {
		t.Errorf("expected ColumnNotNull to be true for 'name' text NOT NULL")
	}
}

func TestSafety_Plan_Hazards(t *testing.T) {
	plan := &Plan{
		TargetSchema: "public",
		Steps: []Step{
			{
				Type:        ChangeDropTable,
				Table:       "legacy_users",
				SQL:         `DROP TABLE "legacy_users";`,
				Destructive: true,
			},
			{
				Type:        ChangeDropColumn,
				Table:       "orders",
				SQL:         `ALTER TABLE "orders" DROP COLUMN "notes";`,
				Destructive: true,
			},
			{
				// SQLite rebuild drop column path
				Type:           ChangeDropColumn,
				Table:          "users",
				SQL:            `CREATE TABLE "_rebuild" (); INSERT INTO "_rebuild" SELECT id FROM users; DROP TABLE "users"; ALTER TABLE "_rebuild" RENAME TO "users";`,
				Destructive:    true,
				IsTableRebuild: true,
			},
			{
				Type:         ChangeAlterColumn,
				Table:        "orders",
				SQL:          `ALTER TABLE "orders" ALTER COLUMN "amount" TYPE smallint;`,
				Destructive:  true,
				TypeNarrowed: true,
			},
			{
				Type:        ChangeAlterColumn,
				Table:       "orders",
				SQL:         `ALTER TABLE "orders" ALTER COLUMN "amount" TYPE bigint;`,
				Destructive: false,
			},
			{
				Type:             ChangeAddColumn,
				Table:            "orders",
				SQL:              `ALTER TABLE "orders" ADD COLUMN "tax" numeric NOT NULL;`,
				ColumnNotNull:    true,
				ColumnHasDefault: false,
			},
			{
				// Column named default_status with NOT NULL: structural detection avoids false negative!
				Type:             ChangeAddColumn,
				Table:            "orders",
				SQL:              `ALTER TABLE "orders" ADD COLUMN "default_status" text NOT NULL;`,
				ColumnNotNull:    true,
				ColumnHasDefault: false,
			},
			{
				Type:             ChangeAddColumn,
				Table:            "orders",
				SQL:              `ALTER TABLE "orders" ADD COLUMN "discount" numeric NOT NULL DEFAULT 0;`,
				ColumnNotNull:    true,
				ColumnHasDefault: true,
			},
			{
				Type:  ChangeCreateIndex,
				Table: "orders",
				SQL:   `CREATE INDEX "idx_orders_tax" ON "orders" ("tax");`,
			},
			{
				Type:  ChangeDropIndex,
				Table: "orders",
				SQL:   `DROP INDEX "idx_old";`,
			},
			{
				Type:  ChangeDropFK,
				Table: "orders",
				SQL:   `ALTER TABLE "orders" DROP CONSTRAINT "fk_old";`,
			},
		},
	}

	hazards := plan.Hazards()
	// Expected hazards:
	// 1. DROP TABLE -> CRITICAL
	// 2. DROP COLUMN (Postgres) -> CRITICAL
	// 3. DROP COLUMN (SQLite rebuild with DROP TABLE) -> CRITICAL (with rebuild description)
	// 4. ALTER COLUMN (destructive type narrowing) -> CRITICAL
	// 5. ALTER COLUMN (safe) -> NOTICE
	// 6. ADD COLUMN "tax" NOT NULL without DEFAULT -> CRITICAL
	// 7. ADD COLUMN "default_status" NOT NULL without DEFAULT -> CRITICAL
	// (ADD COLUMN with DEFAULT 0 -> none)
	// 8. CREATE INDEX -> NOTICE
	// 9. DROP INDEX -> NOTICE
	// 10. DROP FK -> NOTICE
	if len(hazards) != 10 {
		t.Fatalf("expected 10 hazards, got %d: %+v", len(hazards), hazards)
	}

	if hazards[0].Level != HazardLevelCritical || hazards[0].Type != ChangeDropTable || hazards[0].Code != HazardDropTable {
		t.Errorf("expected hazard 0 to be CRITICAL DROP TABLE, got: %+v", hazards[0])
	}
	if hazards[1].Level != HazardLevelCritical || !strings.Contains(hazards[1].Description, "Column on table") || hazards[1].Code != HazardDropColumn {
		t.Errorf("expected hazard 1 to describe dropped column, got: %+v", hazards[1])
	}
	if hazards[2].Level != HazardLevelCritical || !strings.Contains(hazards[2].Description, "dropped and recreated") || hazards[2].Code != HazardDropColumn {
		t.Errorf("expected hazard 2 to describe SQLite table rebuild, got: %+v", hazards[2])
	}
	if hazards[3].Level != HazardLevelCritical || hazards[3].Type != ChangeAlterColumn || hazards[3].Code != HazardTypeNarrow {
		t.Errorf("expected hazard 3 to be CRITICAL ALTER COLUMN, got: %+v", hazards[3])
	}
	if hazards[4].Level != HazardLevelNotice || hazards[4].Type != ChangeAlterColumn {
		t.Errorf("expected hazard 4 to be NOTICE ALTER COLUMN, got: %+v", hazards[4])
	}
	if hazards[5].Level != HazardLevelCritical || hazards[5].Type != ChangeAddColumn || hazards[5].Code != HazardNotNullNoDefault {
		t.Errorf("expected hazard 5 to be CRITICAL ADD COLUMN, got: %+v", hazards[5])
	}
	if hazards[6].Level != HazardLevelCritical || hazards[6].Type != ChangeAddColumn || hazards[6].Code != HazardNotNullNoDefault {
		t.Errorf("expected hazard 6 to be CRITICAL ADD COLUMN for default_status, got: %+v", hazards[6])
	}
	if hazards[7].Level != HazardLevelNotice || hazards[7].Type != ChangeCreateIndex || hazards[7].Code != HazardIndexBuild {
		t.Errorf("expected hazard 7 to be NOTICE CREATE INDEX, got: %+v", hazards[7])
	}
	if hazards[8].Level != HazardLevelNotice || hazards[8].Type != ChangeDropIndex || hazards[8].Code != HazardDropIndex {
		t.Errorf("expected hazard 8 to be NOTICE DROP INDEX, got: %+v", hazards[8])
	}
	if hazards[9].Level != HazardLevelNotice || hazards[9].Type != ChangeDropFK || hazards[9].Code != HazardDropFK {
		t.Errorf("expected hazard 9 to be NOTICE DROP FK, got: %+v", hazards[9])
	}
}

func TestSafety_Plan_Format_Hazards(t *testing.T) {
	plan := &Plan{
		TargetSchema: "public",
		Policy:       DropPolicy{AllowColumn: false},
		Steps: []Step{
			{
				Type:        ChangeDropColumn,
				Table:       "users",
				SQL:         `ALTER TABLE "users" DROP COLUMN "temp";`,
				Destructive: true,
			},
			{
				Type:             ChangeAddColumn,
				Table:            "users",
				SQL:              `ALTER TABLE "users" ADD COLUMN "code" text NOT NULL;`,
				ColumnNotNull:    true,
				ColumnHasDefault: false,
			},
		},
	}

	// Plain text format
	out := plan.String()
	if !strings.Contains(out, "Detected Hazards:") {
		t.Errorf("expected 'Detected Hazards:' section, got: %s", out)
	}
	if !strings.Contains(out, "[CRITICAL]") {
		t.Errorf("expected [CRITICAL] hazard badge, got: %s", out)
	}

	// Color format
	var buf bytes.Buffer
	if err := plan.Format(&buf, true); err != nil {
		t.Fatalf("Format failed: %v", err)
	}
	colorOut := buf.String()
	if !strings.Contains(colorOut, colorRed) {
		t.Errorf("expected colorRed in hazard output, got: %q", colorOut)
	}
}
