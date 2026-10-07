package plan

import (
	"cmp"
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
//
// Ordering is phase-first (by ChangeType priority), then Kahn topological
// ordering within the CREATE_TABLE and DROP_TABLE phases. Dependency cycles
// (legal when FKs are deferred to ADD_FK) are emitted as a deterministically
// sorted strongly-connected remainder — never via a comparator that can
// violate the sort contract.
func SortSteps(steps []Step) {
	priority := map[ChangeType]int{
		ChangeDropFK:             10, // 1. Drop old foreign keys first (unlocks referenced tables)
		ChangeDropCheck:          15, // 1b. Drop obsolete check constraints before redefining them
		ChangeDropIndex:          20, // 2. Drop obsolete indexes
		ChangeDetachPartition:    25, // 2b. Detach partitions before modifying or dropping them
		ChangeDropPolicy:         26, // 2c. Drop policies before recreating
		ChangeDropTrigger:        27, // 2d. Drop triggers before column/table drops or function replaces
		ChangeDropAggregate:      28, // 2e'. Drop aggregates before their support functions (pg_depend)
		ChangeDropView:           28, // 2e. Drop views before underlying table drops
		ChangeDropFunction:       29, // 2f. Drop functions after dependents
		ChangeDropExtension:      29, // 2g. Drop extensions last among early drops
		ChangeCreateExtension:    5,  // 0. Extensions before enums/tables (provides types)
		ChangeCreateEnum:         30, // 3. Create new enum types before tables use them
		ChangeAlterEnum:          35, // 4. Add new enum values before tables insert/alter
		ChangeCreateTable:        40, // 5. Create bare tables (PKs included, FKs deferred; parent tables before partitions)
		ChangeAttachPartition:    42, // 5a. Attach existing tables to partitioned tables
		ChangeRenameColumn:       45, // 5b. Rename columns before adding or altering other columns
		ChangeCreateFunction:     48, // 5c. Functions before triggers/views that call them
		ChangeCreateAggregate:    49, // 5c'. Aggregates after their support functions (pg_depend)
		ChangeAddColumn:          50, // 6. Add new columns
		ChangeAlterColumn:        60, // 7. Modify column types, nullability, defaults
		ChangeCreateIndex:        70, // 8. Build new indexes
		ChangeAddFK:              80, // 9. Add foreign keys (NOT VALID) now that all tables and columns exist
		ChangeAddCheck:           82, // 9b. Add check constraints (NOT VALID) after all tables and columns exist
		ChangeValidateConstraint: 85, // 10. Validate foreign keys and check constraints
		ChangeEnableRLS:          86, // 10a. Enable RLS before creating policies
		ChangeForceRLS:           86,
		ChangeDisableRLS:         86,
		ChangeNoForceRLS:         86,
		ChangeCreatePolicy:       87, // 10b. Policies after RLS enabled
		ChangeCreateTrigger:      88, // 10c. Triggers after functions and columns
		ChangeCommentTable:       89, // 10c'. Comments after all objects exist
		ChangeCommentColumn:      89,
		ChangeCreateView:         95,  // 10d. Views after tables/functions
		ChangeRefreshMatView:     96,  // 10e. Refresh matviews after create
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

	// Compute transitive dependencies (Warshall — correct as-is; do not rewrite).
	for k := range deps {
		for i := range deps {
			if deps[i][k] {
				for j := range deps[k] {
					deps[i][j] = true
				}
			}
		}
	}

	// Phase-stable sort first.
	slices.SortStableFunc(steps, func(a, b Step) int {
		pDiff := priority[a.Type] - priority[b.Type]
		if pDiff != 0 {
			return pDiff
		}
		if a.Schema != b.Schema {
			return strings.Compare(a.Schema, b.Schema)
		}
		if a.Table != b.Table {
			return strings.Compare(a.Table, b.Table)
		}
		return strings.Compare(a.SQL, b.SQL)
	})

	// Within CREATE_TABLE / DROP_TABLE phases, apply Kahn topological order.
	reorderPhase(steps, ChangeCreateTable, deps, false)
	reorderPhase(steps, ChangeDropTable, deps, true)
}

// reorderPhase rewrites the contiguous run of steps of the given type in place
// using Kahn's algorithm over the table dependency graph. reverse=true inverts
// edges (referencing before referenced) for DROP_TABLE. Cycles are emitted as
// a lexically sorted remainder so ordering stays deterministic.
func reorderPhase(steps []Step, typ ChangeType, deps map[string]map[string]bool, reverse bool) {
	start, end := -1, -1
	for i, s := range steps {
		if s.Type == typ {
			if start < 0 {
				start = i
			}
			end = i + 1
		} else if start >= 0 {
			break
		}
	}
	if start < 0 || end-start <= 1 {
		return
	}

	phase := steps[start:end]
	keys := make([]string, len(phase))
	keySet := make(map[string]bool, len(phase))
	for i, s := range phase {
		k := tableKey(s.Schema, s.Table)
		keys[i] = k
		keySet[k] = true
	}

	// Build adjacency among keys present in this phase.
	// Forward (create): edge parent -> child means parent must precede child
	// (child depends on parent). Reverse (drop): edge child -> parent.
	adj := make(map[string][]string)
	indeg := make(map[string]int, len(keySet))
	for k := range keySet {
		indeg[k] = 0
	}
	addEdge := func(from, to string) {
		if from == to || !keySet[from] || !keySet[to] {
			return
		}
		if slices.Contains(adj[from], to) {
			return
		}
		adj[from] = append(adj[from], to)
		indeg[to]++
	}
	for child, parents := range deps {
		if !keySet[child] {
			continue
		}
		for parent := range parents {
			if !keySet[parent] {
				continue
			}
			if reverse {
				addEdge(child, parent) // drop referencing before referenced
			} else {
				addEdge(parent, child) // create referenced before referencing
			}
		}
	}

	// Ready queue: zero-indegree keys, sorted for determinism.
	ready := make([]string, 0, len(keySet))
	for k, d := range indeg {
		if d == 0 {
			ready = append(ready, k)
		}
	}
	slices.Sort(ready)

	order := make([]string, 0, len(keySet))
	for len(ready) > 0 {
		k := ready[0]
		ready = ready[1:]
		order = append(order, k)
		var newly []string
		for _, to := range adj[k] {
			indeg[to]--
			if indeg[to] == 0 {
				newly = append(newly, to)
			}
		}
		slices.Sort(newly)
		ready = append(ready, newly...)
		slices.Sort(ready)
	}

	// Cycle remainder: emit remaining keys in lexical order.
	if len(order) < len(keySet) {
		var rem []string
		for k, d := range indeg {
			if d > 0 {
				rem = append(rem, k)
			}
		}
		slices.Sort(rem)
		order = append(order, rem...)
	}

	// Stable group steps by key in Kahn order, preserving relative order
	// among steps that share a key (shouldn't normally happen).
	byKey := make(map[string][]Step, len(keySet))
	for _, s := range phase {
		k := tableKey(s.Schema, s.Table)
		byKey[k] = append(byKey[k], s)
	}
	out := make([]Step, 0, len(phase))
	for _, k := range order {
		out = append(out, byKey[k]...)
	}
	// Defensive: any leftover (should be empty)
	if len(out) < len(phase) {
		seen := make(map[string]bool, len(order))
		for _, k := range order {
			seen[k] = true
		}
		var leftover []Step
		for _, s := range phase {
			k := tableKey(s.Schema, s.Table)
			if !seen[k] {
				leftover = append(leftover, s)
			}
		}
		slices.SortStableFunc(leftover, func(a, b Step) int {
			return cmp.Or(
				strings.Compare(a.Schema, b.Schema),
				strings.Compare(a.Table, b.Table),
				strings.Compare(a.SQL, b.SQL),
			)
		})
		out = append(out, leftover...)
	}
	copy(steps[start:end], out)
}
