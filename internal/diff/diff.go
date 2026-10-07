package diff

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
	"github.com/muandane/grizzle/internal/scope"
)

// Change represents a pure difference between live and desired database schemas.
type Change struct {
	Type              plan.ChangeType
	Schema            string
	Table             string
	Column            *schema.Column
	OldColumn         *schema.Column
	Index             *schema.Index
	OldIndex          *schema.Index
	ForeignKey        *schema.ForeignKey
	Check             *schema.CheckConstraint
	Enum              *schema.Enum
	OldEnum           *schema.Enum
	EnumValue         string
	TableData         *schema.Table
	Destructive       bool
	IsRenameCandidate bool

	// Structural flags for hazard analysis
	ColumnNotNull    bool
	ColumnHasDefault bool

	// Column change flags
	TypeChanged      bool
	TypeNarrowed     bool
	NullChanged      bool
	DefaultChanged   bool
	GeneratedChanged bool

	// Unmanaged object dependencies (views, triggers, functions depending on this table/column)
	UnmanagedDeps []string

	// Partitioning metadata
	ParentTable      string
	PartitionBounds  string
	ParentHasDefault bool
	IsPendingDetach  bool
}

// Diff compares live and desired schemas using the given scope filters and returns pure changes.
func Diff(live, desired *schema.Schema, targetSchema, shadowSchema string, filters scope.Filters) ([]Change, error) {
	mapping := make(map[string]string)
	if shadowSchema != "" {
		mapping[shadowSchema] = targetSchema
	}
	return DiffWithMappings(live, desired, targetSchema, shadowSchema, filters, mapping)
}

// DiffWithMappings compares live and desired schemas using schemaMappings for normalization and returns pure changes.
//
//nolint:revive // DiffWithMappings is distinct from Diff for multi-schema mapping context
func DiffWithMappings(live, desired *schema.Schema, targetSchema, shadowSchema string, filters scope.Filters, schemaMappings map[string]string) ([]Change, error) {
	var changes []Change

	normalize := func(def string) string {
		return schema.NormalizeDefinitionWithMappings(def, schemaMappings, targetSchema)
	}

	// 1. Custom ENUM Types Diff
	dEnumNames := slices.Collect(maps.Keys(desired.Enums))
	slices.Sort(dEnumNames)
	for _, dName := range dEnumNames {
		dEnum := desired.Enums[dName]
		lEnum, exists := live.Enums[dName]
		if !exists {
			changes = append(changes, Change{
				Type:        plan.ChangeCreateEnum,
				Schema:      targetSchema,
				Table:       dName,
				Enum:        dEnum,
				Destructive: false,
			})
		} else {
			for _, val := range dEnum.Values {
				if !slices.Contains(lEnum.Values, val) {
					changes = append(changes, Change{
						Type:        plan.ChangeAlterEnum,
						Schema:      targetSchema,
						Table:       dName,
						Enum:        dEnum,
						OldEnum:     lEnum,
						EnumValue:   val,
						Destructive: false,
					})
				}
			}
		}
	}

	// Validate partition structural rules on desired tables
	for _, dTable := range desired.Tables {
		if err := validatePartitionStructuralRules(dTable); err != nil {
			return nil, err
		}
	}

	// 2. Tables & Columns Diff
	desiredTableNames := slices.Collect(maps.Keys(desired.Tables))
	slices.Sort(desiredTableNames)
	for _, tblName := range desiredTableNames {
		dTable := desired.Tables[tblName]
		if !scope.IsTableManaged(tblName, filters) {
			continue
		}
		lTable, exists := live.Tables[tblName]
		if !exists {
			tblCopy := *dTable
			if len(dTable.ForeignKeys) > 0 {
				tblCopy.ForeignKeys = make(map[string]*schema.ForeignKey, len(dTable.ForeignKeys))
				for k, v := range dTable.ForeignKeys {
					fkCopy := *v
					if mapped, ok := schemaMappings[fkCopy.RefSchema]; ok {
						fkCopy.RefSchema = mapped
					}
					fkCopy.Definition = normalize(fkCopy.Definition)
					tblCopy.ForeignKeys[k] = &fkCopy
				}
			}
			createChange := Change{
				Type:        plan.ChangeCreateTable,
				Schema:      targetSchema,
				Table:       tblName,
				TableData:   &tblCopy,
				Destructive: false,
			}
			if dTable.IsPartition() {
				createChange.ParentTable = dTable.PartitionOf.Parent
				createChange.PartitionBounds = normalize(dTable.PartitionOf.Bounds)
			}
			changes = append(changes, createChange)

			for _, dIdx := range dTable.Indexes {
				normDef := normalize(dIdx.Definition)
				idxCopy := *dIdx
				idxCopy.Definition = normDef
				changes = append(changes, Change{
					Type:        plan.ChangeCreateIndex,
					Schema:      targetSchema,
					Table:       tblName,
					Index:       &idxCopy,
					Destructive: false,
				})
			}

			for _, dFK := range tblCopy.ForeignKeys {
				changes = append(changes, Change{
					Type:        plan.ChangeAddFK,
					Schema:      targetSchema,
					Table:       tblName,
					ForeignKey:  dFK,
					Destructive: false,
				})
			}

			for _, dCheck := range dTable.Checks {
				checkCopy := *dCheck
				checkCopy.Definition = normalize(dCheck.Definition)
				changes = append(changes, Change{
					Type:        plan.ChangeAddCheck,
					Schema:      targetSchema,
					Table:       tblName,
					Check:       &checkCopy,
					Destructive: false,
				})
			}
			continue
		}

		// Reject in-place table <-> partitioned table conversion
		if lTable.IsPartitioned() != dTable.IsPartitioned() {
			return nil, fmt.Errorf("%w: table %q cannot be converted in-place between regular and partitioned table", plan.ErrPartitionConversion, tblName)
		}
		if lTable.IsPartitioned() && dTable.IsPartitioned() {
			lKey := normalize(lTable.PartitionKey.Def)
			dKey := normalize(dTable.PartitionKey.Def)
			if lTable.PartitionKey.Strategy != dTable.PartitionKey.Strategy || lKey != dKey {
				return nil, fmt.Errorf("%w: table %q cannot change partition strategy or key in-place", plan.ErrPartitionConversion, tblName)
			}
		}

		// Partition attachment or detachment transitions
		if dTable.IsPartition() && !lTable.IsPartition() {
			normBounds := normalize(dTable.PartitionOf.Bounds)
			changes = append(changes, Change{
				Type:            plan.ChangeAttachPartition,
				Schema:          targetSchema,
				Table:           tblName,
				ParentTable:     dTable.PartitionOf.Parent,
				PartitionBounds: normBounds,
				Destructive:     false,
			})
		} else if !dTable.IsPartition() && lTable.IsPartition() {
			changes = append(changes, Change{
				Type:             plan.ChangeDetachPartition,
				Schema:           targetSchema,
				Table:            tblName,
				ParentTable:      lTable.PartitionOf.Parent,
				ParentHasDefault: hasDefaultPartition(lTable.PartitionOf.Parent, live) || hasDefaultPartition(lTable.PartitionOf.Parent, desired),
				IsPendingDetach:  lTable.PartitionOf.IsDetachPending,
				Destructive:      false,
			})
		} else if dTable.IsPartition() && lTable.IsPartition() {
			if lTable.PartitionOf.IsDetachPending {
				changes = append(changes, Change{
					Type:             plan.ChangeDetachPartition,
					Schema:           targetSchema,
					Table:            tblName,
					ParentTable:      lTable.PartitionOf.Parent,
					ParentHasDefault: hasDefaultPartition(lTable.PartitionOf.Parent, live) || hasDefaultPartition(lTable.PartitionOf.Parent, desired),
					IsPendingDetach:  true,
					Destructive:      false,
				})
			}
			normLiveBounds := normalize(lTable.PartitionOf.Bounds)
			normDesiredBounds := normalize(dTable.PartitionOf.Bounds)
			if lTable.PartitionOf.Parent != dTable.PartitionOf.Parent || normLiveBounds != normDesiredBounds {
				changes = append(changes, Change{
					Type:             plan.ChangeDetachPartition,
					Schema:           targetSchema,
					Table:            tblName,
					ParentTable:      lTable.PartitionOf.Parent,
					ParentHasDefault: hasDefaultPartition(lTable.PartitionOf.Parent, live) || hasDefaultPartition(lTable.PartitionOf.Parent, desired),
					IsPendingDetach:  lTable.PartitionOf.IsDetachPending,
					Destructive:      false,
				})
				changes = append(changes, Change{
					Type:            plan.ChangeAttachPartition,
					Schema:          targetSchema,
					Table:           tblName,
					ParentTable:     dTable.PartitionOf.Parent,
					PartitionBounds: normDesiredBounds,
					Destructive:     false,
				})
			}
		}

		// Attached partitions inherit and manage columns through the parent partitioned table
		if !dTable.IsPartition() {
			// Existing table: Diff columns
			addedCols := make(map[string]*schema.Column)
			for colName, dCol := range dTable.Columns {
				lCol, colExists := lTable.Columns[colName]
				if !colExists {
					addedCols[colName] = dCol
				} else {
					typeChanged := schema.NormalizeType(dCol.DataType) != schema.NormalizeType(lCol.DataType)
					nullChanged := dCol.IsNullable != lCol.IsNullable
					defChanged := dCol.DefaultValue != lCol.DefaultValue
					genChanged := false
					if (lCol.Generated == nil) != (dCol.Generated == nil) {
						genChanged = true
					} else if lCol.Generated != nil && dCol.Generated != nil {
						if schema.NormalizeGeneratedExpr(lCol.Generated.Expr) != schema.NormalizeGeneratedExpr(dCol.Generated.Expr) ||
							lCol.Generated.Stored != dCol.Generated.Stored {
							genChanged = true
						}
					}

					if typeChanged || nullChanged || defChanged || genChanged {
						typeNarrowed := typeChanged && schema.IsTypeNarrowing(lCol.DataType, dCol.DataType)
						destructive := typeNarrowed || (!dCol.IsNullable && lCol.IsNullable)
						var unmDeps []string
						if typeChanged || typeNarrowed {
							unmDeps = findUnmanagedDeps(live.Unmanaged, tblName, colName)
						}
						changes = append(changes, Change{
							Type:             plan.ChangeAlterColumn,
							Schema:           targetSchema,
							Table:            tblName,
							Column:           dCol,
							OldColumn:        lCol,
							TypeChanged:      typeChanged,
							TypeNarrowed:     typeNarrowed,
							NullChanged:      nullChanged,
							DefaultChanged:   defChanged,
							GeneratedChanged: genChanged,
							Destructive:      destructive,
							ColumnNotNull:    !dCol.IsNullable,
							ColumnHasDefault: dCol.DefaultValue != "",
							UnmanagedDeps:    unmDeps,
						})
					}
				}
			}

			// Dropped columns
			droppedCols := make(map[string]*schema.Column)
			for colName, lCol := range lTable.Columns {
				if _, inDesired := dTable.Columns[colName]; !inDesired {
					droppedCols[colName] = lCol
				}
			}

			// Check for explicit rename mappings from filters.Renames
			droppedKeys := slices.Collect(maps.Keys(droppedCols))
			slices.Sort(droppedKeys)

			for _, lColName := range droppedKeys {
				lCol := droppedCols[lColName]
				var mappedNew string
				if target, ok := filters.Renames[fmt.Sprintf("%s.%s", tblName, lColName)]; ok {
					mappedNew = target
				} else if target, ok := filters.Renames[lColName]; ok {
					mappedNew = target
				}

				if mappedNew != "" {
					if dCol, ok := addedCols[mappedNew]; ok {
						// Disambiguated rename match found!
						delete(droppedCols, lColName)
						delete(addedCols, mappedNew)

						if filters.ExpandContract {
							// Staged Expand phase: add new column (forced nullable during expand), keep old column in place
							expandedCol := *dCol
							expandedCol.IsNullable = true
							changes = append(changes, Change{
								Type:             plan.ChangeAddColumn,
								Schema:           targetSchema,
								Table:            tblName,
								Column:           &expandedCol,
								Destructive:      false,
								ColumnNotNull:    false,
								ColumnHasDefault: dCol.DefaultValue != "",
							})
						} else {
							// Single-step atomic rename
							changes = append(changes, Change{
								Type:        plan.ChangeRenameColumn,
								Schema:      targetSchema,
								Table:       tblName,
								Column:      dCol,
								OldColumn:   lCol,
								Destructive: false,
							})
						}
					}
				}
			}

			// Process remaining added columns in sorted order
			addedKeys := slices.Collect(maps.Keys(addedCols))
			slices.Sort(addedKeys)
			for _, colName := range addedKeys {
				dCol := addedCols[colName]
				changes = append(changes, Change{
					Type:             plan.ChangeAddColumn,
					Schema:           targetSchema,
					Table:            tblName,
					Column:           dCol,
					Destructive:      false,
					ColumnNotNull:    !dCol.IsNullable,
					ColumnHasDefault: dCol.DefaultValue != "",
				})
			}

			// Process remaining dropped columns (detecting unmapped ambiguous rename candidates)
			remainingDropped := slices.Collect(maps.Keys(droppedCols))
			slices.Sort(remainingDropped)
			for _, colName := range remainingDropped {
				lCol := droppedCols[colName]
				isAmbiguousCandidate := false
				for _, dCol := range addedCols {
					if schema.NormalizeType(dCol.DataType) == schema.NormalizeType(lCol.DataType) {
						isAmbiguousCandidate = true
						break
					}
				}
				changes = append(changes, Change{
					Type:              plan.ChangeDropColumn,
					Schema:            targetSchema,
					Table:             tblName,
					Column:            lCol,
					OldColumn:         lCol,
					Destructive:       true,
					IsRenameCandidate: isAmbiguousCandidate,
					UnmanagedDeps:     findUnmanagedDeps(live.Unmanaged, tblName, colName),
				})
			}
		}

		// Indexes Diff
		for idxName, dIdx := range dTable.Indexes {
			normDef := normalize(dIdx.Definition)
			lIdx, inLive := lTable.Indexes[idxName]
			if !inLive {
				idxCopy := *dIdx
				idxCopy.Definition = normDef
				changes = append(changes, Change{
					Type:        plan.ChangeCreateIndex,
					Schema:      targetSchema,
					Table:       tblName,
					Index:       &idxCopy,
					Destructive: false,
				})
			} else {
				normLive := normalize(lIdx.Definition)
				isInvalid := !lIdx.IsValid
				if normDef != normLive || isInvalid {
					changes = append(changes, Change{
						Type:        plan.ChangeDropIndex,
						Schema:      targetSchema,
						Table:       tblName,
						Index:       lIdx,
						Destructive: !isInvalid,
					})
					idxCopy := *dIdx
					idxCopy.Definition = normDef
					changes = append(changes, Change{
						Type:        plan.ChangeCreateIndex,
						Schema:      targetSchema,
						Table:       tblName,
						Index:       &idxCopy,
						Destructive: false,
					})
				}
			}
		}

		for idxName, lIdx := range lTable.Indexes {
			if _, inDesired := dTable.Indexes[idxName]; !inDesired {
				changes = append(changes, Change{
					Type:        plan.ChangeDropIndex,
					Schema:      targetSchema,
					Table:       tblName,
					Index:       lIdx,
					Destructive: lIdx.IsValid,
				})
			}
		}

		// Foreign Keys Diff
		for fkName, dFK := range dTable.ForeignKeys {
			normDef := normalize(dFK.Definition)
			lFK, inLive := lTable.ForeignKeys[fkName]
			if !inLive {
				fkCopy := *dFK
				fkCopy.Definition = normDef
				changes = append(changes, Change{
					Type:        plan.ChangeAddFK,
					Schema:      targetSchema,
					Table:       tblName,
					ForeignKey:  &fkCopy,
					Destructive: false,
				})
			} else {
				normLive := normalize(lFK.Definition)
				baseLive := strings.TrimSuffix(normLive, " NOT VALID")
				baseDef := strings.TrimSuffix(normDef, " NOT VALID")
				if baseDef != baseLive {
					changes = append(changes, Change{
						Type:        plan.ChangeDropFK,
						Schema:      targetSchema,
						Table:       tblName,
						ForeignKey:  lFK,
						Destructive: true,
					})
					fkCopy := *dFK
					fkCopy.Definition = normDef
					changes = append(changes, Change{
						Type:        plan.ChangeAddFK,
						Schema:      targetSchema,
						Table:       tblName,
						ForeignKey:  &fkCopy,
						Destructive: false,
					})
				} else if !lFK.IsValid {
					fkCopy := *lFK
					fkCopy.Definition = baseLive
					changes = append(changes, Change{
						Type:        plan.ChangeValidateConstraint,
						Schema:      targetSchema,
						Table:       tblName,
						ForeignKey:  &fkCopy,
						Destructive: false,
					})
				}
			}
		}

		for fkName, lFK := range lTable.ForeignKeys {
			if _, inDesired := dTable.ForeignKeys[fkName]; !inDesired {
				changes = append(changes, Change{
					Type:        plan.ChangeDropFK,
					Schema:      targetSchema,
					Table:       tblName,
					ForeignKey:  lFK,
					Destructive: true,
				})
			}
		}

		// Check Constraints Diff. Only locally declared CHECK constraints are
		// managed: constraints inherited from a partitioned parent are invisible
		// here (conislocal = false) and are managed through the parent table.
		for checkName, dCheck := range dTable.Checks {
			normDef := normalize(dCheck.Definition)
			lCheck, inLive := lTable.Checks[checkName]
			if !inLive {
				checkCopy := *dCheck
				checkCopy.Definition = normDef
				changes = append(changes, Change{
					Type:        plan.ChangeAddCheck,
					Schema:      targetSchema,
					Table:       tblName,
					Check:       &checkCopy,
					Destructive: false,
				})
			} else {
				normLive := normalize(lCheck.Definition)
				baseLive := strings.TrimSuffix(normLive, " NOT VALID")
				baseDef := strings.TrimSuffix(normDef, " NOT VALID")
				if baseDef != baseLive {
					changes = append(changes, Change{
						Type:        plan.ChangeDropCheck,
						Schema:      targetSchema,
						Table:       tblName,
						Check:       lCheck,
						Destructive: true,
					})
					checkCopy := *dCheck
					checkCopy.Definition = normDef
					changes = append(changes, Change{
						Type:        plan.ChangeAddCheck,
						Schema:      targetSchema,
						Table:       tblName,
						Check:       &checkCopy,
						Destructive: false,
					})
				} else if !lCheck.IsValid {
					checkCopy := *lCheck
					checkCopy.Definition = baseLive
					changes = append(changes, Change{
						Type:        plan.ChangeValidateConstraint,
						Schema:      targetSchema,
						Table:       tblName,
						Check:       &checkCopy,
						Destructive: false,
					})
				}
			}
		}

		for checkName, lCheck := range lTable.Checks {
			if _, inDesired := dTable.Checks[checkName]; !inDesired {
				// PostgreSQL auto-names CHECK constraints declared with inline
				// CHECK syntax. Auto-named orphans are never auto-dropped:
				// they are indistinguishable from system-generated conversion
				// artifacts (e.g. the partition-bound check left behind by
				// DETACH PARTITION ... CONCURRENTLY). Explicitly named
				// constraints are fully managed, including drops.
				if schema.IsAutoGeneratedCheckName(lTable.Name, lCheck.Name, lTable.Columns) {
					continue
				}
				changes = append(changes, Change{
					Type:        plan.ChangeDropCheck,
					Schema:      targetSchema,
					Table:       tblName,
					Check:       lCheck,
					Destructive: true,
				})
			}
		}
	}

	// 3. Detect dropped tables
	liveTableNames := slices.Collect(maps.Keys(live.Tables))
	slices.Sort(liveTableNames)
	for _, tblName := range liveTableNames {
		if !scope.IsTableManaged(tblName, filters) {
			continue
		}
		if _, exists := desired.Tables[tblName]; !exists {
			lTable := live.Tables[tblName]
			tableSchema := targetSchema
			if lTable != nil && lTable.Schema != "" {
				tableSchema = lTable.Schema
			}
			tableName := tblName
			if lTable != nil && lTable.Name != "" {
				tableName = lTable.Name
			}

			if lTable != nil && len(lTable.ForeignKeys) > 0 {
				fkNames := slices.Collect(maps.Keys(lTable.ForeignKeys))
				slices.Sort(fkNames)
				for _, fkName := range fkNames {
					lFK := lTable.ForeignKeys[fkName]
					changes = append(changes, Change{
						Type:        plan.ChangeDropFK,
						Schema:      tableSchema,
						Table:       tableName,
						ForeignKey:  lFK,
						Destructive: true,
					})
				}
			}

			changes = append(changes, Change{
				Type:          plan.ChangeDropTable,
				Schema:        tableSchema,
				Table:         tableName,
				Destructive:   true,
				UnmanagedDeps: findUnmanagedDeps(live.Unmanaged, tblName, ""),
			})
		}
	}

	return changes, nil
}

func findUnmanagedDeps(unmanaged map[string]*schema.UnmanagedObject, table, column string) []string {
	if len(unmanaged) == 0 {
		return nil
	}
	var deps []string
	for _, obj := range unmanaged {
		for _, ref := range obj.DependsOn {
			if ref.Table == table {
				if column == "" || ref.Column == "" || ref.Column == column {
					depStr := fmt.Sprintf("%s:%s", obj.Kind, obj.Name)
					if ref.Column != "" {
						depStr += fmt.Sprintf(" (column %s.%s)", ref.Table, ref.Column)
					}
					deps = append(deps, depStr)
					break
				}
			}
		}
	}
	slices.Sort(deps)
	return deps
}

func extractPartitionColumns(def string) []string {
	clean := strings.TrimSpace(def)
	if open := strings.Index(clean, "("); open != -1 {
		if close := strings.LastIndex(clean, ")"); close > open {
			clean = clean[open+1 : close]
		}
	} else {
		for _, prefix := range []string{"RANGE", "LIST", "HASH"} {
			if strings.HasPrefix(strings.ToUpper(clean), prefix) {
				clean = strings.TrimSpace(clean[len(prefix):])
			}
		}
	}
	parts := strings.Split(clean, ",")
	var cols []string
	for _, p := range parts {
		col := strings.Trim(strings.TrimSpace(p), `"'`+"`")
		if col != "" {
			cols = append(cols, col)
		}
	}
	return cols
}

func extractIndexColumns(def string) []string {
	if def == "" {
		return nil
	}
	upper := strings.ToUpper(def)
	onIdx := strings.Index(upper, " ON ")
	if onIdx == -1 {
		return nil
	}
	parenStart := strings.Index(def[onIdx:], "(")
	if parenStart == -1 {
		return nil
	}
	start := onIdx + parenStart
	depth := 0
	end := -1
	for i := start; i < len(def); i++ {
		if def[i] == '(' {
			depth++
		} else if def[i] == ')' {
			depth--
			if depth == 0 {
				end = i
				break
			}
		}
	}
	if end == -1 {
		return nil
	}
	colsStr := def[start+1 : end]
	var rawCols []string
	depth = 0
	last := 0
	for i := 0; i < len(colsStr); i++ {
		if colsStr[i] == '(' {
			depth++
		} else if colsStr[i] == ')' {
			depth--
		} else if colsStr[i] == ',' && depth == 0 {
			rawCols = append(rawCols, colsStr[last:i])
			last = i + 1
		}
	}
	rawCols = append(rawCols, colsStr[last:])

	var cols []string
	for _, raw := range rawCols {
		raw = strings.TrimSpace(raw)
		parts := strings.Fields(raw)
		if len(parts) > 0 {
			col := strings.Trim(parts[0], `"'`+"`")
			if col != "" {
				cols = append(cols, col)
			}
		}
	}
	return cols
}

func validatePartitionStructuralRules(t *schema.Table) error {
	if t == nil || !t.IsPartitioned() {
		return nil
	}
	partCols := extractPartitionColumns(t.PartitionKey.Def)
	if len(partCols) == 0 {
		return nil
	}

	// 1. Primary key must include all partition key columns
	if t.PrimaryKey != nil && len(t.PrimaryKey.Columns) > 0 {
		for _, pCol := range partCols {
			if !slices.Contains(t.PrimaryKey.Columns, pCol) {
				return fmt.Errorf("%w: primary key on partitioned table %q (%v) must include partition key column %q",
					plan.ErrPartitionKeyNotInUnique, t.Name, t.PrimaryKey.Columns, pCol)
			}
		}
	}

	// 2. Any unique index must include all partition key columns
	for idxName, idx := range t.Indexes {
		if idx.IsUnique {
			idxCols := extractIndexColumns(idx.Definition)
			if len(idxCols) > 0 {
				for _, pCol := range partCols {
					if !slices.Contains(idxCols, pCol) {
						return fmt.Errorf("%w: unique index %q on partitioned table %q (%v) must include partition key column %q",
							plan.ErrPartitionKeyNotInUnique, idxName, t.Name, idxCols, pCol)
					}
				}
			}
		}
	}
	return nil
}

func hasDefaultPartition(parentTable string, s *schema.Schema) bool {
	if s == nil || parentTable == "" {
		return false
	}
	for _, tbl := range s.Tables {
		if tbl.IsPartition() && tbl.PartitionOf.Parent == parentTable {
			b := strings.ToUpper(strings.TrimSpace(tbl.PartitionOf.Bounds))
			if b == "DEFAULT" || strings.Contains(b, "DEFAULT") {
				return true
			}
		}
	}
	return false
}
