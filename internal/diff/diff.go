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
	Table             string
	Column            *schema.Column
	OldColumn         *schema.Column
	Index             *schema.Index
	OldIndex          *schema.Index
	ForeignKey        *schema.ForeignKey
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
	TypeChanged    bool
	TypeNarrowed   bool
	NullChanged    bool
	DefaultChanged bool

	// Unmanaged object dependencies (views, triggers, functions depending on this table/column)
	UnmanagedDeps []string
}

// Diff compares live and desired schemas using the given scope filters and returns pure changes.
func Diff(live, desired *schema.Schema, targetSchema, shadowSchema string, filters scope.Filters) []Change {
	var changes []Change

	// 1. Custom ENUM Types Diff
	for dName, dEnum := range desired.Enums {
		lEnum, exists := live.Enums[dName]
		if !exists {
			changes = append(changes, Change{
				Type:        plan.ChangeCreateEnum,
				Table:       dName,
				Enum:        dEnum,
				Destructive: false,
			})
		} else {
			for _, val := range dEnum.Values {
				if !slices.Contains(lEnum.Values, val) {
					changes = append(changes, Change{
						Type:        plan.ChangeAlterEnum,
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

	// 2. Tables & Columns Diff
	for tblName, dTable := range desired.Tables {
		if !scope.IsTableManaged(tblName, filters) {
			continue
		}
		lTable, exists := live.Tables[tblName]
		if !exists {
			changes = append(changes, Change{
				Type:        plan.ChangeCreateTable,
				Table:       tblName,
				TableData:   dTable,
				Destructive: false,
			})

			for _, dIdx := range dTable.Indexes {
				normDef := schema.NormalizeDefinition(dIdx.Definition, shadowSchema, targetSchema)
				idxCopy := *dIdx
				idxCopy.Definition = normDef
				changes = append(changes, Change{
					Type:        plan.ChangeCreateIndex,
					Table:       tblName,
					Index:       &idxCopy,
					Destructive: false,
				})
			}

			for _, dFK := range dTable.ForeignKeys {
				normDef := schema.NormalizeDefinition(dFK.Definition, shadowSchema, targetSchema)
				fkCopy := *dFK
				fkCopy.Definition = normDef
				changes = append(changes, Change{
					Type:        plan.ChangeAddFK,
					Table:       tblName,
					ForeignKey:  &fkCopy,
					Destructive: false,
				})
			}
			continue
		}

		// Existing table: Diff columns
		addedCols := make(map[string]*schema.Column)
		for colName, dCol := range dTable.Columns {
			lCol, colExists := lTable.Columns[colName]
			if !colExists {
				addedCols[colName] = dCol
			} else {
				typeChanged := dCol.DataType != lCol.DataType
				nullChanged := dCol.IsNullable != lCol.IsNullable
				defChanged := dCol.DefaultValue != lCol.DefaultValue

				if typeChanged || nullChanged || defChanged {
					typeNarrowed := typeChanged && schema.IsTypeNarrowing(lCol.DataType, dCol.DataType)
					destructive := typeNarrowed || (!dCol.IsNullable && lCol.IsNullable)
					var unmDeps []string
					if typeChanged || typeNarrowed {
						unmDeps = findUnmanagedDeps(live.Unmanaged, tblName, colName)
					}
					changes = append(changes, Change{
						Type:             plan.ChangeAlterColumn,
						Table:            tblName,
						Column:           dCol,
						OldColumn:        lCol,
						TypeChanged:      typeChanged,
						TypeNarrowed:     typeNarrowed,
						NullChanged:      nullChanged,
						DefaultChanged:   defChanged,
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
				Table:             tblName,
				Column:            lCol,
				OldColumn:         lCol,
				Destructive:       true,
				IsRenameCandidate: isAmbiguousCandidate,
				UnmanagedDeps:     findUnmanagedDeps(live.Unmanaged, tblName, colName),
			})
		}

		// Indexes Diff
		for idxName, dIdx := range dTable.Indexes {
			normDef := schema.NormalizeDefinition(dIdx.Definition, shadowSchema, targetSchema)
			lIdx, inLive := lTable.Indexes[idxName]
			if !inLive {
				idxCopy := *dIdx
				idxCopy.Definition = normDef
				changes = append(changes, Change{
					Type:        plan.ChangeCreateIndex,
					Table:       tblName,
					Index:       &idxCopy,
					Destructive: false,
				})
			} else {
				normLive := schema.NormalizeDefinition(lIdx.Definition, shadowSchema, targetSchema)
				isInvalid := !lIdx.IsValid
				if normDef != normLive || isInvalid {
					changes = append(changes, Change{
						Type:        plan.ChangeDropIndex,
						Table:       tblName,
						Index:       lIdx,
						Destructive: !isInvalid,
					})
					idxCopy := *dIdx
					idxCopy.Definition = normDef
					changes = append(changes, Change{
						Type:        plan.ChangeCreateIndex,
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
					Table:       tblName,
					Index:       lIdx,
					Destructive: lIdx.IsValid,
				})
			}
		}

		// Foreign Keys Diff
		for fkName, dFK := range dTable.ForeignKeys {
			normDef := schema.NormalizeDefinition(dFK.Definition, shadowSchema, targetSchema)
			lFK, inLive := lTable.ForeignKeys[fkName]
			if !inLive {
				fkCopy := *dFK
				fkCopy.Definition = normDef
				changes = append(changes, Change{
					Type:        plan.ChangeAddFK,
					Table:       tblName,
					ForeignKey:  &fkCopy,
					Destructive: false,
				})
			} else {
				normLive := schema.NormalizeDefinition(lFK.Definition, shadowSchema, targetSchema)
				baseLive := strings.TrimSuffix(normLive, " NOT VALID")
				baseDef := strings.TrimSuffix(normDef, " NOT VALID")
				if baseDef != baseLive {
					changes = append(changes, Change{
						Type:        plan.ChangeDropFK,
						Table:       tblName,
						ForeignKey:  lFK,
						Destructive: true,
					})
					fkCopy := *dFK
					fkCopy.Definition = normDef
					changes = append(changes, Change{
						Type:        plan.ChangeAddFK,
						Table:       tblName,
						ForeignKey:  &fkCopy,
						Destructive: false,
					})
				} else if !lFK.IsValid {
					fkCopy := *lFK
					fkCopy.Definition = baseLive
					changes = append(changes, Change{
						Type:        plan.ChangeValidateConstraint,
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
					Table:       tblName,
					ForeignKey:  lFK,
					Destructive: true,
				})
			}
		}
	}

	// 3. Detect dropped tables
	for tblName := range live.Tables {
		if !scope.IsTableManaged(tblName, filters) {
			continue
		}
		if _, exists := desired.Tables[tblName]; !exists {
			changes = append(changes, Change{
				Type:          plan.ChangeDropTable,
				Table:         tblName,
				Destructive:   true,
				UnmanagedDeps: findUnmanagedDeps(live.Unmanaged, tblName, ""),
			})
		}
	}

	return changes
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
