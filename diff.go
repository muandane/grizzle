package grizzle

import "fmt"

// diffSchemas compares the live schema with the desired schema and produces a sequenced list of migration steps.
func diffSchemas(live, desired *SchemaIR, targetSchema string) []Step {
	var steps []Step

	// 1. Tables in desired schema
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
			continue
		}

		// 2. Existing table: Diff columns
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
					// Mark destructive if type changed or changing from nullable to NOT NULL
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

		// 3. Detect dropped columns in existing tables
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
	}

	// 4. Detect dropped tables
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

	// 5. Order all steps topologically
	sortSteps(steps)

	return steps
}
