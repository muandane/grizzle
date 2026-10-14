package lint

import (
	"strings"

	"github.com/muandane/grizzle/internal/schema"
)

// reservedSQLKeywords contains common SQL keywords that create syntactic ambiguity,
// require compulsory quoting in DDL/DML, and cause subtle ORM/driver conflicts.
var reservedSQLKeywords = map[string]bool{
	"all":        true,
	"alter":      true,
	"and":        true,
	"as":         true,
	"asc":        true,
	"check":      true,
	"column":     true,
	"constraint": true,
	"create":     true,
	"database":   true,
	"default":    true,
	"delete":     true,
	"desc":       true,
	"distinct":   true,
	"drop":       true,
	"else":       true,
	"foreign":    true,
	"from":       true,
	"group":      true,
	"having":     true,
	"in":         true,
	"index":      true,
	"insert":     true,
	"into":       true,
	"is":         true,
	"join":       true,
	"key":        true,
	"limit":      true,
	"not":        true,
	"null":       true,
	"on":         true,
	"or":         true,
	"order":      true,
	"primary":    true,
	"references": true,
	"select":     true,
	"set":        true,
	"table":      true,
	"to":         true,
	"union":      true,
	"unique":     true,
	"update":     true,
	"user":       true,
	"values":     true,
	"where":      true,
}

// DisallowReservedKeyword (L022) reports tables or columns named after
// SQL reserved keywords. Such names require quoted identifiers ("user", "order")
// across queries and cause accidental syntax errors when unquoted.
type DisallowReservedKeyword struct{}

// ID returns the rule identifier.
func (DisallowReservedKeyword) ID() string { return "L022" }

// Description returns a human-readable rule summary.
func (DisallowReservedKeyword) Description() string {
	return "Tables and columns should not use SQL reserved keywords as identifiers"
}

// Check returns an error diagnostic for each table or column whose name collides
// with an SQL reserved keyword.
func (DisallowReservedKeyword) Check(s *schema.Schema) []Diagnostic {
	var diags []Diagnostic
	for _, tableName := range sortedTableNames(s) {
		t := s.Tables[tableName]
		if t == nil {
			continue
		}
		if reservedSQLKeywords[strings.ToLower(tableName)] {
			diags = append(diags, Diagnostic{
				RuleID:   "L022",
				Severity: SeverityError,
				Table:    tableName,
				Message:  "table name " + tableName + " is a reserved SQL keyword",
			})
		}
		for _, colName := range sortedColumnNames(t) {
			if reservedSQLKeywords[strings.ToLower(colName)] {
				diags = append(diags, Diagnostic{
					RuleID:   "L022",
					Severity: SeverityError,
					Table:    tableName,
					Column:   colName,
					Message:  "column name " + colName + " is a reserved SQL keyword",
				})
			}
		}
	}
	return diags
}
