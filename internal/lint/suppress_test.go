package lint_test

import (
	"testing"

	"github.com/muandane/grizzle/internal/lint"
	"github.com/muandane/grizzle/internal/schema"
)

func TestLint_InlineSuppression(t *testing.T) {
	// Schema with a table missing PK (would trigger L001)
	// but annotated with -- grizzle:lint-ignore L001
	sqlText := `
		-- Top level comment
		-- grizzle:lint-ignore L001
		CREATE TABLE logs (
			created_at TIMESTAMP,
			message TEXT
		);

		CREATE TABLE events (
			id INTEGER,
			event_name TEXT
		);
	`

	s := &schema.Schema{
		Name:      "public",
		SourceSQL: sqlText,
		Tables: map[string]*schema.Table{
			"logs": {
				Name: "logs",
				Columns: map[string]*schema.Column{
					"created_at": {Name: "created_at", DataType: "timestamp"},
					"message":    {Name: "message", DataType: "text"},
				},
			},
			"events": {
				Name: "events",
				Columns: map[string]*schema.Column{
					"id":         {Name: "id", DataType: "integer"},
					"event_name": {Name: "event_name", DataType: "text"},
				},
			},
		},
	}

	diags := lint.Lint(s, lint.MissingPrimaryKey{})

	// logs table L001 is suppressed, events table L001 must be reported!
	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic after suppression, got %d: %+v", len(diags), diags)
	}
	if diags[0].Table != "events" {
		t.Errorf("expected diagnostic for table 'events', got %s", diags[0].Table)
	}
	if diags[0].RuleID != "L001" {
		t.Errorf("expected rule L001, got %s", diags[0].RuleID)
	}
}

func TestLint_GlobalSuppression(t *testing.T) {
	sqlText := `
		-- grizzle:lint-disable L001, L003
		CREATE TABLE logs (
			created_at TIMESTAMP
		);
		CREATE TABLE events (
			created_at TIMESTAMP
		);
	`

	s := &schema.Schema{
		Name:      "public",
		SourceSQL: sqlText,
		Tables: map[string]*schema.Table{
			"logs":   {Name: "logs"},
			"events": {Name: "events"},
		},
	}

	diags := lint.Lint(s, lint.MissingPrimaryKey{})
	if len(diags) != 0 {
		t.Fatalf("expected 0 diagnostics after global suppression, got %d: %+v", len(diags), diags)
	}
}
