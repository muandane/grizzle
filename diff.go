package grizzle

import (
	"fmt"
	"slices"
)

// diffSchemas compares the live schema with the desired schema and produces a sequenced list of migration steps.
func diffSchemas(live, desired *SchemaIR, targetSchema, shadowSchema string) []Step {
	var steps []Step

	// 1. Custom ENUM Types Diff
	for dName, dEnum := range desired.Enums {
		lEnum, exists := live.Enums[dName]
		if !exists {
			// New Enum type
			steps = append(steps, Step{
				Type:        ChangeCreateEnum,
				Table:       dName,
				SQL:         generateCreateEnumSQL(targetSchema, dEnum),
				Destructive: false,
			})
		} else {
			// Existing Enum type: check for appended values
			for _, val := range dEnum.Values {
				if !slices.Contains(lEnum.Values, val) {
					steps = append(steps, Step{
						Type:        ChangeAlterEnum,
						Table:       dName,
						SQL:         generateAddEnumValueSQL(targetSchema, dName, val),
						Destructive: false,
					})
				}
			}
		}
	}

	// 2. Tables & Columns Diff
	for tblName, dTable := range desired.Tables {
		lTable, exists := live.Tables[tblName]
		if !exists {
			// New table to create
			steps = append(steps, Step{
				Type:        ChangeCreateTable,
				Table:       tblName,
				SQL:         generateCreateTableSQL(targetSchema, dTable),
				Destructive: false,
			})

			// All indexes on this new table must be created
			for _, dIdx := range dTable.Indexes {
				normDef := normalizeDefinition(dIdx.Definition, shadowSchema, targetSchema)
				steps = append(steps, Step{
					Type:        ChangeCreateIndex,
					Table:       tblName,
					SQL:         generateCreateIndexSQL(normDef),
					Destructive: false,
				})
			}

			// All foreign keys on this new table must be added
			for fkName, dFK := range dTable.ForeignKeys {
				normDef := normalizeDefinition(dFK.Definition, shadowSchema, targetSchema)
				steps = append(steps, Step{
					Type:        ChangeAddFK,
					Table:       tblName,
					SQL:         generateAddFKSQL(targetSchema, tblName, fkName, normDef),
					Destructive: false,
				})
			}
			continue
		}

		// Existing table: Diff columns
		for colName, dCol := range dTable.Columns {
			lCol, colExists := lTable.Columns[colName]
			if !colExists {
				// Added column
				steps = append(steps, Step{
					Type:        ChangeAddColumn,
					Table:       tblName,
					SQL:         generateAddColumnSQL(targetSchema, tblName, dCol),
					Destructive: false,
				})
			} else {
				// Check for column alterations
				typeChanged := dCol.DataType != lCol.DataType
				nullChanged := dCol.IsNullable != lCol.IsNullable
				defChanged := dCol.DefaultValue != lCol.DefaultValue

				if typeChanged || nullChanged || defChanged {
					destructive := typeChanged || (!dCol.IsNullable && lCol.IsNullable)
					steps = append(steps, Step{
						Type:        ChangeAlterColumn,
						Table:       tblName,
						SQL:         generateAlterColumnSQL(targetSchema, tblName, lCol, dCol),
						Destructive: destructive,
					})
				}
			}
		}

		// Detect dropped columns in existing tables
		for colName := range lTable.Columns {
			if _, exists := dTable.Columns[colName]; !exists {
				steps = append(steps, Step{
					Type:        ChangeDropColumn,
					Table:       tblName,
					SQL:         fmt.Sprintf("ALTER TABLE %q.%q DROP COLUMN %q;", targetSchema, tblName, colName),
					Destructive: true,
				})
			}
		}

		// Existing table: Diff Indexes
		for idxName, dIdx := range dTable.Indexes {
			normDesiredDef := normalizeDefinition(dIdx.Definition, shadowSchema, targetSchema)
			lIdx, idxExists := lTable.Indexes[idxName]
			if !idxExists {
				steps = append(steps, Step{
					Type:        ChangeCreateIndex,
					Table:       tblName,
					SQL:         generateCreateIndexSQL(normDesiredDef),
					Destructive: false,
				})
			} else {
				normLiveDef := normalizeDefinition(lIdx.Definition, shadowSchema, targetSchema)
				if normDesiredDef != normLiveDef {
					// Index definition changed: drop old and create new
					steps = append(steps, Step{
						Type:        ChangeDropIndex,
						Table:       tblName,
						SQL:         generateDropIndexSQL(targetSchema, idxName),
						Destructive: true,
					})
					steps = append(steps, Step{
						Type:        ChangeCreateIndex,
						Table:       tblName,
						SQL:         generateCreateIndexSQL(normDesiredDef),
						Destructive: false,
					})
				}
			}
		}

		for idxName := range lTable.Indexes {
			if _, inDesired := dTable.Indexes[idxName]; !inDesired {
				steps = append(steps, Step{
					Type:        ChangeDropIndex,
					Table:       tblName,
					SQL:         generateDropIndexSQL(targetSchema, idxName),
					Destructive: true,
				})
			}
		}

		// Existing table: Diff Foreign Keys
		for fkName, dFK := range dTable.ForeignKeys {
			normDesiredDef := normalizeDefinition(dFK.Definition, shadowSchema, targetSchema)
			lFK, fkExists := lTable.ForeignKeys[fkName]
			if !fkExists {
				steps = append(steps, Step{
					Type:        ChangeAddFK,
					Table:       tblName,
					SQL:         generateAddFKSQL(targetSchema, tblName, fkName, normDesiredDef),
					Destructive: false,
				})
			} else {
				normLiveDef := normalizeDefinition(lFK.Definition, shadowSchema, targetSchema)
				if normDesiredDef != normLiveDef {
					steps = append(steps, Step{
						Type:        ChangeDropFK,
						Table:       tblName,
						SQL:         generateDropFKSQL(targetSchema, tblName, fkName),
						Destructive: true,
					})
					steps = append(steps, Step{
						Type:        ChangeAddFK,
						Table:       tblName,
						SQL:         generateAddFKSQL(targetSchema, tblName, fkName, normDesiredDef),
						Destructive: false,
					})
				}
			}
		}

		for fkName := range lTable.ForeignKeys {
			if _, inDesired := dTable.ForeignKeys[fkName]; !inDesired {
				steps = append(steps, Step{
					Type:        ChangeDropFK,
					Table:       tblName,
					SQL:         generateDropFKSQL(targetSchema, tblName, fkName),
					Destructive: true,
				})
			}
		}
	}

	// 3. Detect dropped tables
	for tblName := range live.Tables {
		if _, exists := desired.Tables[tblName]; !exists {
			steps = append(steps, Step{
				Type:        ChangeDropTable,
				Table:       tblName,
				SQL:         fmt.Sprintf("DROP TABLE %q.%q CASCADE;", targetSchema, tblName),
				Destructive: true,
			})
		}
	}

	// 4. Order all steps topologically
	sortSteps(steps)

	return steps
}
