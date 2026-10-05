package plan

import "slices"

// SortSteps applies topological ordering so dependent DDL operations execute in the correct relational sequence.
func SortSteps(steps []Step) {
	priority := map[ChangeType]int{
		ChangeDropFK:      10,  // 1. Drop old foreign keys first (unlocks referenced tables)
		ChangeDropIndex:   20,  // 2. Drop obsolete indexes
		ChangeCreateEnum:  30,  // 3. Create new enum types before tables use them
		ChangeAlterEnum:   35,  // 4. Add new enum values before tables insert/alter
		ChangeCreateTable: 40,  // 5. Create bare tables (PKs included, FKs deferred)
		ChangeAddColumn:   50,  // 6. Add new columns
		ChangeAlterColumn: 60,  // 7. Modify column types, nullability, defaults
		ChangeCreateIndex:        70,  // 8. Build new indexes
		ChangeAddFK:              80,  // 9. Add foreign keys (NOT VALID) now that all tables and columns exist
		ChangeValidateConstraint: 85,  // 10. Validate foreign keys
		ChangeDropColumn:         90,  // 11. Drop columns (if allowed)
		ChangeDropTable:          100, // 12. Drop tables (if allowed)
	}

	slices.SortStableFunc(steps, func(a, b Step) int {
		return priority[a.Type] - priority[b.Type]
	})
}
