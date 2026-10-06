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
