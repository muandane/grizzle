package diff_test

import (
	"errors"
	"testing"

	"github.com/muandane/grizzle/internal/diff"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
	"github.com/muandane/grizzle/internal/scope"
)

func TestDiff_PureUnit(t *testing.T) {
	filters := scope.Filters{}

	// 1. Create table
	liveEmpty := &schema.Schema{Tables: make(map[string]*schema.Table), Enums: make(map[string]*schema.Enum)}
	desiredTable := &schema.Schema{
		Tables: map[string]*schema.Table{
			"users": {
				Name: "users",
				Columns: map[string]*schema.Column{
					"id": {Name: "id", DataType: "bigint", IsNullable: false},
				},
				Indexes:     make(map[string]*schema.Index),
				ForeignKeys: make(map[string]*schema.ForeignKey),
			},
		},
		Enums: make(map[string]*schema.Enum),
	}

	changes, err := diff.Diff(liveEmpty, desiredTable, "shadow", "public", filters)
	if err != nil {
		t.Fatalf("diff failed: %v", err)
	}
	if len(changes) == 0 || changes[0].Type != plan.ChangeCreateTable {
		t.Fatalf("expected ChangeCreateTable, got: %+v", changes)
	}

	// 2. Drop table
	changesDrop, err := diff.Diff(desiredTable, liveEmpty, "shadow", "public", filters)
	if err != nil {
		t.Fatalf("diff failed: %v", err)
	}
	if len(changesDrop) == 0 || changesDrop[0].Type != plan.ChangeDropTable {
		t.Fatalf("expected ChangeDropTable, got: %+v", changesDrop)
	}

	// 3. Add & Drop column
	liveWithCol := &schema.Schema{
		Tables: map[string]*schema.Table{
			"users": {
				Name: "users",
				Columns: map[string]*schema.Column{
					"id":    {Name: "id", DataType: "bigint", IsNullable: false},
					"email": {Name: "email", DataType: "varchar(255)", IsNullable: false},
				},
				Indexes:     make(map[string]*schema.Index),
				ForeignKeys: make(map[string]*schema.ForeignKey),
			},
		},
		Enums: make(map[string]*schema.Enum),
	}

	// Adding email column
	changesAddCol, err := diff.Diff(desiredTable, liveWithCol, "shadow", "public", filters)
	if err != nil {
		t.Fatalf("diff failed: %v", err)
	}
	var foundAddCol bool
	for _, c := range changesAddCol {
		if c.Type == plan.ChangeAddColumn && c.Column.Name == "email" {
			foundAddCol = true
			if !c.ColumnNotNull {
				t.Errorf("expected ColumnNotNull to be true")
			}
		}
	}
	if !foundAddCol {
		t.Fatalf("expected ChangeAddColumn for email, got: %+v", changesAddCol)
	}

	// Dropping email column
	changesDropCol, err := diff.Diff(liveWithCol, desiredTable, "shadow", "public", filters)
	if err != nil {
		t.Fatalf("diff failed: %v", err)
	}
	var foundDropCol bool
	for _, c := range changesDropCol {
		if c.Type == plan.ChangeDropColumn && c.OldColumn.Name == "email" {
			foundDropCol = true
			if !c.Destructive {
				t.Errorf("expected drop column to be destructive")
			}
		}
	}
	if !foundDropCol {
		t.Fatalf("expected ChangeDropColumn for email, got: %+v", changesDropCol)
	}

	// 4. Alter column (type change)
	liveAltered := &schema.Schema{
		Tables: map[string]*schema.Table{
			"users": {
				Name: "users",
				Columns: map[string]*schema.Column{
					"id": {Name: "id", DataType: "integer", IsNullable: false}, // narrowed from bigint
				},
				Indexes:     make(map[string]*schema.Index),
				ForeignKeys: make(map[string]*schema.ForeignKey),
			},
		},
		Enums: make(map[string]*schema.Enum),
	}
	changesAlter, err := diff.Diff(desiredTable, liveAltered, "shadow", "public", filters)
	if err != nil {
		t.Fatalf("diff failed: %v", err)
	}
	var foundAlter bool
	for _, c := range changesAlter {
		if c.Type == plan.ChangeAlterColumn {
			foundAlter = true
			if !c.TypeNarrowed {
				t.Errorf("expected TypeNarrowed to be true when converting bigint to integer")
			}
		}
	}
	if !foundAlter {
		t.Fatalf("expected ChangeAlterColumn, got: %+v", changesAlter)
	}

	// 5. Enums (Create and Alter)
	liveEnum := &schema.Schema{
		Tables: make(map[string]*schema.Table),
		Enums: map[string]*schema.Enum{
			"status": {Name: "status", Values: []string{"active"}},
		},
	}
	desiredEnum := &schema.Schema{
		Tables: make(map[string]*schema.Table),
		Enums: map[string]*schema.Enum{
			"status": {Name: "status", Values: []string{"active", "archived"}},
			"role":   {Name: "role", Values: []string{"admin", "user"}},
		},
	}
	changesEnum, err := diff.Diff(liveEnum, desiredEnum, "shadow", "public", filters)
	if err != nil {
		t.Fatalf("diff failed: %v", err)
	}
	var foundCreateRole, foundAlterStatus bool
	for _, c := range changesEnum {
		if c.Type == plan.ChangeCreateEnum && c.Enum.Name == "role" {
			foundCreateRole = true
		}
		if c.Type == plan.ChangeAlterEnum && c.Enum.Name == "status" && c.EnumValue == "archived" {
			foundAlterStatus = true
		}
	}
	if !foundCreateRole || !foundAlterStatus {
		t.Fatalf("expected enum changes, got: %+v", changesEnum)
	}

	// 6. Indexes (Add, Drop, Invalid repair)
	liveIdx := &schema.Schema{
		Tables: map[string]*schema.Table{
			"users": {
				Name: "users",
				Columns: map[string]*schema.Column{
					"id": {Name: "id", DataType: "bigint", IsNullable: false},
				},
				Indexes: map[string]*schema.Index{
					"idx_users_id": {Name: "idx_users_id", Definition: "CREATE INDEX idx_users_id ON users (id)", IsValid: false},
				},
				ForeignKeys: make(map[string]*schema.ForeignKey),
			},
		},
		Enums: make(map[string]*schema.Enum),
	}
	desiredIdx := &schema.Schema{
		Tables: map[string]*schema.Table{
			"users": {
				Name: "users",
				Columns: map[string]*schema.Column{
					"id": {Name: "id", DataType: "bigint", IsNullable: false},
				},
				Indexes: map[string]*schema.Index{
					"idx_users_id": {Name: "idx_users_id", Definition: "CREATE INDEX idx_users_id ON users (id)", IsValid: true},
				},
				ForeignKeys: make(map[string]*schema.ForeignKey),
			},
		},
		Enums: make(map[string]*schema.Enum),
	}
	changesIdx, err := diff.Diff(liveIdx, desiredIdx, "shadow", "public", filters)
	if err != nil {
		t.Fatalf("diff failed: %v", err)
	}
	var foundDropIdx, foundAddIdx bool
	for _, c := range changesIdx {
		if c.Type == plan.ChangeDropIndex {
			foundDropIdx = true
		}
		if c.Type == plan.ChangeCreateIndex {
			foundAddIdx = true
		}
	}
	if !foundDropIdx || !foundAddIdx {
		t.Fatalf("expected invalid index recovery (DROP + CREATE), got: %+v", changesIdx)
	}
}

func TestDiff_PartitionedTables(t *testing.T) {
	filters := scope.Filters{}

	// 1. Creation of partitioned table + partition
	desired := &schema.Schema{
		Tables: map[string]*schema.Table{
			"measurements": {
				Name: "measurements",
				Columns: map[string]*schema.Column{
					"city_id":  {Name: "city_id", DataType: "integer", IsNullable: false},
					"log_date": {Name: "log_date", DataType: "date", IsNullable: false},
				},
				PartitionKey: &schema.PartitionKey{
					Strategy: schema.PartitionStrategyRange,
					Def:      "RANGE (log_date)",
				},
				Indexes:     make(map[string]*schema.Index),
				ForeignKeys: make(map[string]*schema.ForeignKey),
			},
			"measurements_p1": {
				Name: "measurements_p1",
				Columns: map[string]*schema.Column{
					"city_id":  {Name: "city_id", DataType: "integer", IsNullable: false},
					"log_date": {Name: "log_date", DataType: "date", IsNullable: false},
				},
				PartitionOf: &schema.PartitionOf{
					Parent: "measurements",
					Bounds: "FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')",
				},
				Indexes:     make(map[string]*schema.Index),
				ForeignKeys: make(map[string]*schema.ForeignKey),
			},
		},
		Enums: make(map[string]*schema.Enum),
	}

	changes, err := diff.Diff(&schema.Schema{Tables: make(map[string]*schema.Table)}, desired, "public", "_shadow", filters)
	if err != nil {
		t.Fatalf("diff creation failed: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("expected 2 changes, got %d", len(changes))
	}
	var parentChange, childChange *diff.Change
	for i := range changes {
		if changes[i].Table == "measurements" {
			parentChange = &changes[i]
		}
		if changes[i].Table == "measurements_p1" {
			childChange = &changes[i]
		}
	}
	if parentChange == nil || parentChange.Type != plan.ChangeCreateTable {
		t.Errorf("expected parent table create change, got: %+v", parentChange)
	}
	if childChange == nil || childChange.Type != plan.ChangeCreateTable || childChange.ParentTable != "measurements" {
		t.Errorf("expected child partition create change with parent, got: %+v", childChange)
	}

	// 2. Attach existing table to partitioned table
	liveStandalone := &schema.Schema{
		Tables: map[string]*schema.Table{
			"measurements": {
				Name: "measurements",
				Columns: map[string]*schema.Column{
					"city_id":  {Name: "city_id", DataType: "integer", IsNullable: false},
					"log_date": {Name: "log_date", DataType: "date", IsNullable: false},
				},
				PartitionKey: &schema.PartitionKey{
					Strategy: schema.PartitionStrategyRange,
					Def:      "RANGE (log_date)",
				},
				Indexes:     make(map[string]*schema.Index),
				ForeignKeys: make(map[string]*schema.ForeignKey),
			},
			"measurements_p1": {
				Name: "measurements_p1",
				Columns: map[string]*schema.Column{
					"city_id":  {Name: "city_id", DataType: "integer", IsNullable: false},
					"log_date": {Name: "log_date", DataType: "date", IsNullable: false},
				},
				Indexes:     make(map[string]*schema.Index),
				ForeignKeys: make(map[string]*schema.ForeignKey),
			},
		},
		Enums: make(map[string]*schema.Enum),
	}

	attachChanges, err := diff.Diff(liveStandalone, desired, "public", "_shadow", filters)
	if err != nil {
		t.Fatalf("attach diff failed: %v", err)
	}
	if len(attachChanges) != 1 || attachChanges[0].Type != plan.ChangeAttachPartition {
		t.Fatalf("expected ChangeAttachPartition, got: %+v", attachChanges)
	}
	if attachChanges[0].ParentTable != "measurements" {
		t.Errorf("expected ParentTable to be measurements, got: %s", attachChanges[0].ParentTable)
	}

	// 3. Detach partition to standalone table
	detachChanges, err := diff.Diff(desired, liveStandalone, "public", "_shadow", filters)
	if err != nil {
		t.Fatalf("detach diff failed: %v", err)
	}
	if len(detachChanges) != 1 || detachChanges[0].Type != plan.ChangeDetachPartition {
		t.Fatalf("expected ChangeDetachPartition, got: %+v", detachChanges)
	}
	if detachChanges[0].ParentTable != "measurements" {
		t.Errorf("expected ParentTable to be measurements, got: %s", detachChanges[0].ParentTable)
	}

	// 4. Reject in-place regular table -> partitioned table conversion
	liveRegular := &schema.Schema{
		Tables: map[string]*schema.Table{
			"measurements": {
				Name: "measurements",
				Columns: map[string]*schema.Column{
					"city_id":  {Name: "city_id", DataType: "integer", IsNullable: false},
					"log_date": {Name: "log_date", DataType: "date", IsNullable: false},
				},
				Indexes:     make(map[string]*schema.Index),
				ForeignKeys: make(map[string]*schema.ForeignKey),
			},
		},
		Enums: make(map[string]*schema.Enum),
	}
	_, err = diff.Diff(liveRegular, desired, "public", "_shadow", filters)
	if err == nil {
		t.Fatalf("expected in-place partitioning conversion to fail, got nil")
	}
	if !errors.Is(err, plan.ErrPartitionConversion) {
		t.Errorf("expected ErrPartitionConversion, got: %v", err)
	}
}

func TestDiff_PartialAndFunctionalIndexes(t *testing.T) {
	filters := scope.Filters{}

	tests := []struct {
		name          string
		liveDef       string
		desiredDef    string
		liveValid     bool
		expectedDrops int
		expectedAdds  int
	}{
		{
			name:          "identical functional index with shadow schema qualifiers",
			liveDef:       "CREATE UNIQUE INDEX idx_email ON public.users USING btree (lower((email)::text))",
			desiredDef:    "CREATE UNIQUE INDEX idx_email ON _shadow.users USING btree (lower((email)::text))",
			liveValid:     true,
			expectedDrops: 0,
			expectedAdds:  0,
		},
		{
			name:          "identical partial index with shadow schema qualifiers",
			liveDef:       "CREATE INDEX idx_orders ON public.orders USING btree (created_at) WHERE ((status)::text = 'pending'::text)",
			desiredDef:    "CREATE INDEX idx_orders ON _shadow.orders USING btree (created_at) WHERE ((status)::text = 'pending'::text)",
			liveValid:     true,
			expectedDrops: 0,
			expectedAdds:  0,
		},
		{
			name:          "partial index predicate changed",
			liveDef:       "CREATE INDEX idx_orders ON public.orders USING btree (created_at) WHERE ((status)::text = 'pending'::text)",
			desiredDef:    "CREATE INDEX idx_orders ON _shadow.orders USING btree (created_at) WHERE ((status)::text = 'complete'::text)",
			liveValid:     true,
			expectedDrops: 1,
			expectedAdds:  1,
		},
		{
			name:          "functional index expression changed",
			liveDef:       "CREATE INDEX idx_email ON public.users USING btree (lower((email)::text))",
			desiredDef:    "CREATE INDEX idx_email ON _shadow.users USING btree (upper((email)::text))",
			liveValid:     true,
			expectedDrops: 1,
			expectedAdds:  1,
		},
		{
			name:          "invalid partial index triggers recovery",
			liveDef:       "CREATE INDEX idx_orders ON public.orders USING btree (created_at) WHERE ((status)::text = 'pending'::text)",
			desiredDef:    "CREATE INDEX idx_orders ON _shadow.orders USING btree (created_at) WHERE ((status)::text = 'pending'::text)",
			liveValid:     false,
			expectedDrops: 1,
			expectedAdds:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			live := &schema.Schema{
				Tables: map[string]*schema.Table{
					"t": {
						Name: "t",
						Columns: map[string]*schema.Column{
							"id": {Name: "id", DataType: "integer"},
						},
						Indexes: map[string]*schema.Index{
							"idx": {Name: "idx", Definition: tt.liveDef, IsValid: tt.liveValid},
						},
						ForeignKeys: make(map[string]*schema.ForeignKey),
					},
				},
				Enums: make(map[string]*schema.Enum),
			}
			desired := &schema.Schema{
				Tables: map[string]*schema.Table{
					"t": {
						Name: "t",
						Columns: map[string]*schema.Column{
							"id": {Name: "id", DataType: "integer"},
						},
						Indexes: map[string]*schema.Index{
							"idx": {Name: "idx", Definition: tt.desiredDef, IsValid: true},
						},
						ForeignKeys: make(map[string]*schema.ForeignKey),
					},
				},
				Enums: make(map[string]*schema.Enum),
			}

			changes, err := diff.Diff(live, desired, "public", "_shadow", filters)
			if err != nil {
				t.Fatalf("unexpected diff error: %v", err)
			}

			var drops, adds int
			for _, c := range changes {
				if c.Type == plan.ChangeDropIndex {
					drops++
				}
				if c.Type == plan.ChangeCreateIndex {
					adds++
				}
			}

			if drops != tt.expectedDrops || adds != tt.expectedAdds {
				t.Errorf("got %d drops, %d adds; want %d drops, %d adds", drops, adds, tt.expectedDrops, tt.expectedAdds)
			}
		})
	}
}

func TestDiff_MultiSchemaCrossSchemaFK(t *testing.T) {
	mappings := map[string]string{
		"_shadow_identity": "identity",
		"_shadow_billing":  "billing",
	}

	live := &schema.Schema{
		Name:      "billing",
		Tables:    make(map[string]*schema.Table),
		Enums:     make(map[string]*schema.Enum),
		Unmanaged: make(map[string]*schema.UnmanagedObject),
	}

	desired := &schema.Schema{
		Name:      "billing",
		Tables:    make(map[string]*schema.Table),
		Enums:     make(map[string]*schema.Enum),
		Unmanaged: make(map[string]*schema.UnmanagedObject),
	}

	// Add accounts table in billing referencing identity.users
	desired.Tables["accounts"] = &schema.Table{
		Schema: "billing",
		Name:   "accounts",
		Columns: map[string]*schema.Column{
			"id":      {Name: "id", DataType: "bigint", Position: 1},
			"user_id": {Name: "user_id", DataType: "bigint", Position: 2},
		},
		Indexes: make(map[string]*schema.Index),
		ForeignKeys: map[string]*schema.ForeignKey{
			"fk_user": {
				Name:       "fk_user",
				TableName:  "accounts",
				RefSchema:  "identity",
				RefTable:   "users",
				Definition: "FOREIGN KEY (user_id) REFERENCES _shadow_identity.users(id)",
				IsValid:    true,
			},
		},
	}

	changes, err := diff.DiffWithMappings(live, desired, "billing", "_shadow_billing", scope.Filters{}, mappings)
	if err != nil {
		t.Fatalf("unexpected diff error: %v", err)
	}

	var hasCreateTable, hasAddFK bool
	for _, c := range changes {
		if c.Type == plan.ChangeCreateTable && c.Table == "accounts" && c.Schema == "billing" {
			hasCreateTable = true
		}
		if c.Type == plan.ChangeAddFK && c.Table == "accounts" && c.Schema == "billing" {
			hasAddFK = true
			if c.ForeignKey.Definition != "FOREIGN KEY (user_id) REFERENCES identity.users(id)" {
				t.Errorf("FK definition not normalized: got %q", c.ForeignKey.Definition)
			}
		}
	}

	if !hasCreateTable {
		t.Errorf("expected ChangeCreateTable for billing.accounts")
	}
	if !hasAddFK {
		t.Errorf("expected ChangeAddFK for billing.accounts")
	}
}
