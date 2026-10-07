package plan

import (
	"slices"
	"strings"
)

func tableKey(schemaName, tableName string) string {
	if schemaName != "" {
		return schemaName + "." + tableName
	}
	return tableName
}

// SortSteps applies topological ordering so dependent DDL operations execute in the correct relational sequence.
// Cross-schema foreign keys and intra-schema foreign keys are topologically sorted:
// referenced tables precede referencing tables on creation; reverse on drop.
func SortSteps(steps []Step) {
	priority := map[ChangeType]int{
		ChangeDropFK:             10,  // 1. Drop old foreign keys first (unlocks referenced tables)
		ChangeDropCheck:          15,  // 1b. Drop obsolete check constraints before redefining them
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
		ChangeAddCheck:           82,  // 9b. Add check constraints (NOT VALID) after all tables and columns exist
		ChangeValidateConstraint: 85,  // 10. Validate foreign keys and check constraints
		ChangeDropColumn:         90,  // 11. Drop columns (if allowed)
		ChangeDropTable:          100, // 12. Drop tables (if allowed)
	}

	// Build relational dependency graph: referencingTable -> referencedTable
	// deps[A][B] = true means table A depends on table B (A references B)
	deps := make(map[string]map[string]bool)
	addDep := func(child, parent string) {
		if child == "" || parent == "" || child == parent {
			return
		}
		if deps[child] == nil {
			deps[child] = make(map[string]bool)
		}
		deps[child][parent] = true
	}

	for _, s := range steps {
		childKey := tableKey(s.Schema, s.Table)
		// 1. From ChangeCreateTable DependsOn
		for _, dep := range s.DependsOn {
			addDep(childKey, dep)
			addDep(s.Table, dep)
		}
		// 2. From ChangeAddFK RefTable
		if s.Type == ChangeAddFK && s.RefTable != "" {
			addDep(childKey, s.RefTable)
			addDep(s.Table, s.RefTable)
		}
		// 3. From partition hierarchy: child partition depends on parent partitioned table
		if s.ParentTable != "" {
			parentKey := s.ParentTable
			if s.Schema != "" && !strings.Contains(s.ParentTable, ".") {
				parentKey = s.Schema + "." + s.ParentTable
			}
			addDep(childKey, parentKey)
			addDep(s.Table, s.ParentTable)
		}
	}

	// Compute transitive dependencies
	for k := range deps {
		for i := range deps {
			if deps[i][k] {
				for j := range deps[k] {
					deps[i][j] = true
				}
			}
		}
	}

	isDep := func(child, parent string) bool {
		if deps[child] != nil && deps[child][parent] {
			return true
		}
		return false
	}

	slices.SortStableFunc(steps, func(a, b Step) int {
		pDiff := priority[a.Type] - priority[b.Type]
		if pDiff != 0 {
			return pDiff
		}

		aKey := tableKey(a.Schema, a.Table)
		bKey := tableKey(b.Schema, b.Table)

		if a.Type == ChangeCreateTable && b.Type == ChangeCreateTable {
			// Partition hierarchy: parent table before child partition
			if a.ParentTable == "" && b.ParentTable != "" && (b.ParentTable == a.Table || b.ParentTable == aKey) {
				return -1
			}
			if b.ParentTable == "" && a.ParentTable != "" && (a.ParentTable == b.Table || a.ParentTable == bKey) {
				return 1
			}

			// FK dependency: referenced table before referencing table
			// If a depends on b (a references b), b must precede a -> return 1
			if isDep(aKey, bKey) || isDep(a.Table, bKey) || isDep(aKey, b.Table) || isDep(a.Table, b.Table) {
				return 1
			}
			// If b depends on a (b references a), a must precede b -> return -1
			if isDep(bKey, aKey) || isDep(b.Table, aKey) || isDep(bKey, a.Table) || isDep(b.Table, a.Table) {
				return -1
			}
		}

		if a.Type == ChangeDropTable && b.Type == ChangeDropTable {
			// Partition hierarchy: child partition before parent table
			if a.ParentTable == "" && b.ParentTable != "" && (b.ParentTable == a.Table || b.ParentTable == aKey) {
				return 1
			}
			if b.ParentTable == "" && a.ParentTable != "" && (a.ParentTable == b.Table || a.ParentTable == bKey) {
				return -1
			}

			// FK dependency: referencing table must be dropped BEFORE referenced table
			// If a depends on b (a references b), a must be dropped before b -> return -1
			if isDep(aKey, bKey) || isDep(a.Table, bKey) || isDep(aKey, b.Table) || isDep(a.Table, b.Table) {
				return -1
			}
			// If b depends on a (b references a), b must be dropped before a -> return 1
			if isDep(bKey, aKey) || isDep(b.Table, aKey) || isDep(bKey, a.Table) || isDep(b.Table, a.Table) {
				return 1
			}
		}

		if a.Schema != b.Schema {
			return strings.Compare(a.Schema, b.Schema)
		}
		if a.Table != b.Table {
			return strings.Compare(a.Table, b.Table)
		}
		return strings.Compare(a.SQL, b.SQL)
	})
}
