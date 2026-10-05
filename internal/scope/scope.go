package scope

import (
	"errors"
	"path"
	"slices"
)

// BuiltinIgnoredTables defines database extension and metadata tables that Grizzle must never alter or drop.
var BuiltinIgnoredTables = []string{
	"spatial_ref_sys",
	"geometry_columns",
	"geography_columns",
	"raster_columns",
	"raster_overviews",
	"grizzle_history",
}

// Filters carries the unmanaged-table rules for diff and plan phases.
type Filters struct {
	Includes       []string          `json:"includes,omitempty"`
	Excludes       []string          `json:"excludes,omitempty"`
	Strict         bool              `json:"strict,omitempty"`
	Renames        map[string]string `json:"renames,omitempty"`
	ExpandContract bool              `json:"expand_contract,omitempty"`
}

// Validate checks whether filter options conform to strict scoping requirements.
func (f Filters) Validate() error {
	if f.Strict && len(f.Includes) == 0 {
		return errors.New("grizzle: strict scope violation: IncludeTables cannot be empty when StrictScope is enabled")
	}
	return nil
}

// IsTableManaged returns true if the table is managed by Grizzle according to the given filters.
func IsTableManaged(tableName string, filters Filters) bool {
	// Built-in system and GIS extension tables that must never be altered or dropped
	if slices.Contains(BuiltinIgnoredTables, tableName) {
		return false
	}

	// User-defined Exclude patterns
	for _, pattern := range filters.Excludes {
		matched, err := path.Match(pattern, tableName)
		if err != nil {
			// If pattern contains malformed glob syntax (e.g. unclosed '['),
			// fall back to literal comparison so the protection rule is not silently disabled.
			if pattern == tableName {
				return false
			}
			continue
		}
		if matched || pattern == tableName {
			return false
		}
	}

	// User-defined Include patterns (if specified, only matching tables are managed)
	if len(filters.Includes) > 0 {
		for _, pattern := range filters.Includes {
			matched, err := path.Match(pattern, tableName)
			if err != nil {
				if pattern == tableName {
					return true
				}
				continue
			}
			if matched || pattern == tableName {
				return true
			}
		}
		return false
	}

	return true
}
