package plan

import (
	"fmt"
	"strings"
)

// ChangeType describes the category of a schema mutation.
type ChangeType string

const (
	ChangeCreateEnum  ChangeType = "CREATE_ENUM"
	ChangeAlterEnum   ChangeType = "ALTER_ENUM"
	ChangeCreateTable ChangeType = "CREATE_TABLE"
	ChangeDropTable   ChangeType = "DROP_TABLE"
	ChangeAddColumn   ChangeType = "ADD_COLUMN"
	ChangeDropColumn  ChangeType = "DROP_COLUMN"
	ChangeAlterColumn ChangeType = "ALTER_COLUMN"
	ChangeCreateIndex ChangeType = "CREATE_INDEX"
	ChangeDropIndex   ChangeType = "DROP_INDEX"
	ChangeAddFK       ChangeType = "ADD_FK"
	ChangeDropFK      ChangeType = "DROP_FK"
)

// Step represents a single atomic DDL migration statement.
type Step struct {
	Type        ChangeType `json:"type"`
	Table       string     `json:"table"`
	SQL         string     `json:"sql"`
	Destructive bool       `json:"destructive,omitzero"`

	// Structural column metadata for precise hazard analysis
	ColumnNotNull    bool `json:"column_not_null,omitzero"`
	ColumnHasDefault bool `json:"column_has_default,omitzero"`
}

// DropPolicy defines fine-grained permissions for destructive operations.
type DropPolicy struct {
	AllowTable  bool `json:"allow_table"`
	AllowColumn bool `json:"allow_column"`
	AllowIndex  bool `json:"allow_index"`
	AllowFK     bool `json:"allow_fk"`
}

// IsAllowed checks if a given migration step is permitted by the policy.
func (p DropPolicy) IsAllowed(s Step) bool {
	if !s.Destructive {
		return true
	}
	switch s.Type {
	case ChangeDropTable:
		return p.AllowTable
	case ChangeDropColumn:
		return p.AllowColumn
	case ChangeDropIndex:
		return p.AllowIndex
	case ChangeDropFK:
		return p.AllowFK
	case ChangeAlterColumn:
		return p.AllowColumn
	default:
		return false
	}
}

// Plan contains the complete list of sequenced migration steps.
type Plan struct {
	TargetSchema string     `json:"target_schema"`
	Steps        []Step     `json:"steps"`
	Policy       DropPolicy `json:"policy"`
}

// HasDestructive reports whether any step in the plan is destructive.
func (p *Plan) HasDestructive() bool {
	for _, s := range p.Steps {
		if s.Destructive {
			return true
		}
	}
	return false
}

// Additions returns the number of newly created resources.
func (p *Plan) Additions() int {
	count := 0
	for _, s := range p.Steps {
		switch s.Type {
		case ChangeCreateEnum, ChangeCreateTable, ChangeAddColumn, ChangeCreateIndex, ChangeAddFK:
			count++
		}
	}
	return count
}

// Modifications returns the number of modified resources.
func (p *Plan) Modifications() int {
	count := 0
	for _, s := range p.Steps {
		switch s.Type {
		case ChangeAlterColumn, ChangeAlterEnum:
			count++
		}
	}
	return count
}

// Deletions returns the number of dropped resources.
func (p *Plan) Deletions() int {
	count := 0
	for _, s := range p.Steps {
		switch s.Type {
		case ChangeDropTable, ChangeDropColumn, ChangeDropIndex, ChangeDropFK:
			count++
		}
	}
	return count
}

// Blocked returns the count of steps blocked by the current DropPolicy.
func (p *Plan) Blocked() int {
	count := 0
	for _, s := range p.Steps {
		if !p.Policy.IsAllowed(s) {
			count++
		}
	}
	return count
}

// Summary returns the total counts of additions, modifications, deletions, and policy-blocked steps.
func (p *Plan) Summary() (adds, alters, drops, blocked int) {
	return p.Additions(), p.Modifications(), p.Deletions(), p.Blocked()
}

// HazardLevel indicates the operational or data-loss severity of a migration step.
type HazardLevel string

const (
	// HazardLevelCritical indicates potential data loss (e.g. DROP TABLE, DROP COLUMN).
	HazardLevelCritical HazardLevel = "CRITICAL"
	// HazardLevelWarning indicates execution risk (e.g. NOT NULL without DEFAULT on existing table).
	HazardLevelWarning HazardLevel = "WARNING"
	// HazardLevelNotice indicates table locking or performance implications (e.g. index build).
	HazardLevelNotice HazardLevel = "NOTICE"
)

// Hazard describes an operational risk detected in a planned migration step.
type Hazard struct {
	Level       HazardLevel `json:"level"`
	Type        ChangeType  `json:"type"`
	Table       string      `json:"table"`
	Description string      `json:"description"`
	SQL         string      `json:"sql"`
}

// Hazards analyzes all planned steps and returns detected operational and data-loss risks.
func (p *Plan) Hazards() []Hazard {
	var hazards []Hazard
	for _, s := range p.Steps {
		switch s.Type {
		case ChangeDropTable:
			hazards = append(hazards, Hazard{
				Level:       HazardLevelCritical,
				Type:        s.Type,
				Table:       s.Table,
				Description: fmt.Sprintf("Table %q will be dropped with all its data and dependent objects", s.Table),
				SQL:         s.SQL,
			})
		case ChangeDropColumn:
			desc := fmt.Sprintf("Column on table %q will be dropped with all existing row values", s.Table)
			if strings.Contains(strings.ToUpper(s.SQL), "DROP TABLE") {
				// SQLite rebuild path: whole table is dropped and recreated.
				desc = fmt.Sprintf("Table %q will be dropped and recreated; only matching columns are copied back", s.Table)
			}
			hazards = append(hazards, Hazard{
				Level:       HazardLevelCritical,
				Type:        s.Type,
				Table:       s.Table,
				Description: desc,
				SQL:         s.SQL,
			})
		case ChangeAlterColumn:
			level := HazardLevelNotice
			desc := fmt.Sprintf("Column on table %q will be modified", s.Table)
			if s.Destructive {
				level = HazardLevelCritical
				desc = fmt.Sprintf("Column on table %q has a destructive type change that may cause data loss or truncation", s.Table)
			}
			hazards = append(hazards, Hazard{
				Level:       level,
				Type:        s.Type,
				Table:       s.Table,
				Description: desc,
				SQL:         s.SQL,
			})
		case ChangeAddColumn:
			hasWarning := s.ColumnNotNull && !s.ColumnHasDefault
			if !hasWarning && !s.ColumnNotNull && !s.ColumnHasDefault {
				upperSQL := strings.ToUpper(s.SQL)
				if strings.Contains(upperSQL, "NOT NULL") && !strings.Contains(upperSQL, "DEFAULT") {
					hasWarning = true
				}
			}
			if hasWarning {
				hazards = append(hazards, Hazard{
					Level:       HazardLevelWarning,
					Type:        s.Type,
					Table:       s.Table,
					Description: fmt.Sprintf("Adding NOT NULL column without DEFAULT to existing table %q will fail if the table contains rows", s.Table),
					SQL:         s.SQL,
				})
			}
		case ChangeCreateIndex:
			hazards = append(hazards, Hazard{
				Level:       HazardLevelNotice,
				Type:        s.Type,
				Table:       s.Table,
				Description: fmt.Sprintf("Index creation on table %q acquires a ShareLock unless created concurrently", s.Table),
				SQL:         s.SQL,
			})
		case ChangeDropIndex:
			hazards = append(hazards, Hazard{
				Level:       HazardLevelNotice,
				Type:        s.Type,
				Table:       s.Table,
				Description: fmt.Sprintf("Dropping index on table %q may degrade active query performance", s.Table),
				SQL:         s.SQL,
			})
		case ChangeDropFK:
			hazards = append(hazards, Hazard{
				Level:       HazardLevelNotice,
				Type:        s.Type,
				Table:       s.Table,
				Description: fmt.Sprintf("Dropping foreign key constraint on table %q removes referential integrity enforcement", s.Table),
				SQL:         s.SQL,
			})
		}
	}
	return hazards
}
