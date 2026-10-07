package grizzle

import (
	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/dialect/sqlite"
	"github.com/muandane/grizzle/internal/diff"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
	"github.com/muandane/grizzle/internal/scope"
)

// Test-only ANSI helpers for white-box format assertions.
// Production rendering lives in internal/plan/visualize.go.
const (
	colorReset  = "\033[0m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorRed    = "\033[31m"
	colorBold   = "\033[1m"
	colorCyan   = "\033[36m"
)

// tableFilters bridges scope filters for white-box root tests.
type tableFilters struct {
	includes []string
	excludes []string
}

func (tf tableFilters) toScope() scope.Filters {
	return scope.Filters{
		Includes: tf.includes,
		Excludes: tf.excludes,
	}
}

// diffSchemas renders Postgres steps for white-box root tests.
func diffSchemas(live, desired *schema.Schema, targetSchema, shadowSchema string, filters ...any) []plan.Step {
	var f scope.Filters
	if len(filters) > 0 {
		if tf, ok := filters[0].(tableFilters); ok {
			f = tf.toScope()
		} else if sf, ok := filters[0].(scope.Filters); ok {
			f = sf
		}
	}
	changes, err := diff.Diff(live, desired, targetSchema, shadowSchema, f)
	if err != nil {
		panic(err)
	}
	return postgres.RenderChanges(targetSchema, changes)
}

// diffSQLiteSchemas renders SQLite steps for white-box root tests.
func diffSQLiteSchemas(live, desired *schema.Schema, filters ...any) []plan.Step {
	var f scope.Filters
	if len(filters) > 0 {
		if tf, ok := filters[0].(tableFilters); ok {
			f = tf.toScope()
		} else if sf, ok := filters[0].(scope.Filters); ok {
			f = sf
		}
	}
	return sqlite.Diff(live, desired, f)
}

// DetectDialectFromDriver bridges the internal driver-type matcher for
// white-box dialect detection tests.
var DetectDialectFromDriver = detectDialectFromDriver

// DetectDialectForTest bridges the full detection path (driver match +
// probe fallback) for white-box tests.
var DetectDialectForTest = detectDialect
