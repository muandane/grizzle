package diff

import (
	"slices"

	"github.com/yourorg/grizzle/internal/plan"
	"github.com/yourorg/grizzle/internal/schema"
	"github.com/yourorg/grizzle/internal/scope"
)

// Change represents a pure difference between live and desired database schemas.
type Change struct {
	Type        plan.ChangeType
	Table       string
	Column      *schema.Column
	OldColumn   *schema.Column
	Index       *schema.Index
	OldIndex    *schema.Index
	ForeignKey  *schema.ForeignKey
	Enum        *schema.Enum
	OldEnum     *schema.Enum
	EnumValue   string
	TableData   *schema.Table
	Destructive bool

	// Structural flags for hazard analysis
	ColumnNotNull    bool
	ColumnHasDefault bool

	// Column change flags
	TypeChanged    bool
	NullChanged    bool
	DefaultChanged bool
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
		for colName, dCol := range dTable.Columns {
			lCol, colExists := lTable.Columns[colName]
			if !colExists {
				changes = append(changes, Change{
					Type:             plan.ChangeAddColumn,
					Table:            tblName,
					Column:           dCol,
					Destructive:      false,
					ColumnNotNull:    !dCol.IsNullable,
					ColumnHasDefault: dCol.DefaultValue != "",
				})
			} else {
				typeChanged := dCol.DataType != lCol.DataType
				nullChanged := dCol.IsNullable != lCol.IsNullable
				defChanged := dCol.DefaultValue != lCol.DefaultValue

				if typeChanged || nullChanged || defChanged {
					destructive := typeChanged || (!dCol.IsNullable && lCol.IsNullable)
					changes = append(changes, Change{
						Type:             plan.ChangeAlterColumn,
						Table:            tblName,
						Column:           dCol,
						OldColumn:        lCol,
						TypeChanged:      typeChanged,
						NullChanged:      nullChanged,
						DefaultChanged:   defChanged,
						Destructive:      destructive,
						ColumnNotNull:    !dCol.IsNullable,
						ColumnHasDefault: dCol.DefaultValue != "",
					})
				}
			}
		}

		// Dropped columns
		for colName, lCol := range lTable.Columns {
			if _, inDesired := dTable.Columns[colName]; !inDesired {
				changes = append(changes, Change{
					Type:        plan.ChangeDropColumn,
					Table:       tblName,
					Column:      lCol,
					Destructive: true,
				})
			}
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
				if normDef != normLive {
					changes = append(changes, Change{
						Type:        plan.ChangeDropIndex,
						Table:       tblName,
						Index:       lIdx,
						Destructive: true,
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
					Destructive: true,
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
				if normDef != normLive {
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
				Type:        plan.ChangeDropTable,
				Table:       tblName,
				Destructive: true,
			})
		}
	}

	return changes
}
