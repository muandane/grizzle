package plan

import (
	"slices"
	"strings"
)

// SortSteps applies topological ordering so dependent DDL operations execute in the correct relational sequence.
func SortSteps(steps []Step) {
	priority := map[ChangeType]int{
		ChangeDropFK:             10,  // 1. Drop old foreign keys first (unlocks referenced tables)
		ChangeDropIndex:          20,  // 2. Drop obsolete indexes
		ChangeDetachPartition:    25,  // 2b. Detach partitions before modifying or dropping them
		ChangeCreateEnum:         30,  // 3. Create new enum types before tables use them
		ChangeAlterEnum:          35,  // 4. Add new enum values before tables insert/alter
		ChangeCreateTable:        40,  // 5. Create bare tables (PKs included, FKs deferred; parent tables before partitions)
		ChangeAttachPartition:    42,  // 5a. Attach existing tables to partitioned tables
		ChangeRenameColumn:       45,  // 5b. Rename columns before adding or altering other columns
		ChangeAddColumn:          50,  // 6. Add new columns
		ChangeAlterColumn:        60,  // 7. Modify column types, nullability, defaults
		ChangeCreateIndex:        70,  // 8. Build new indexes
		ChangeAddFK:              80,  // 9. Add foreign keys (NOT VALID) now that all tables and columns exist
		ChangeValidateConstraint: 85,  // 10. Validate foreign keys
		ChangeDropColumn:         90,  // 11. Drop columns (if allowed)
		ChangeDropTable:          100, // 12. Drop tables (if allowed)
	}

	slices.SortStableFunc(steps, func(a, b Step) int {
		pDiff := priority[a.Type] - priority[b.Type]
		if pDiff != 0 {
			return pDiff
		}
		// If both are ChangeCreateTable, ensure parent partitioned tables come before child partitions
		if a.Type == ChangeCreateTable && b.Type == ChangeCreateTable {
			if a.ParentTable == "" && b.ParentTable != "" {
				return -1
			}
			if a.ParentTable != "" && b.ParentTable == "" {
				return 1
			}
			if b.ParentTable == a.Table {
				return -1
			}
			if a.ParentTable == b.Table {
				return 1
			}
		}
		if a.Table != b.Table {
			return strings.Compare(a.Table, b.Table)
		}
		return strings.Compare(a.SQL, b.SQL)
	})
}
