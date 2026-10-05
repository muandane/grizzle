package schema

// Schema represents the parsed relational structure of a database schema.
type Schema struct {
	Name      string                      `json:"name"`
	Tables    map[string]*Table           `json:"tables"`
	Enums     map[string]*Enum            `json:"enums"`
	Unmanaged map[string]*UnmanagedObject `json:"unmanaged,omitempty"`
}

// UnmanagedKind specifies the type of an unmanaged database object.
type UnmanagedKind string

const (
	// UnmanagedView indicates a standard SQL VIEW.
	UnmanagedView UnmanagedKind = "VIEW"
	// UnmanagedMaterialized indicates a MATERIALIZED VIEW.
	UnmanagedMaterialized UnmanagedKind = "MATERIALIZED_VIEW"
	// UnmanagedTrigger indicates a database trigger attached to a table.
	UnmanagedTrigger UnmanagedKind = "TRIGGER"
	// UnmanagedFunction indicates a stored procedure or function.
	UnmanagedFunction UnmanagedKind = "FUNCTION"
	// UnmanagedSequence indicates an unmanaged database sequence.
	UnmanagedSequence UnmanagedKind = "SEQUENCE"
	// UnmanagedEnum indicates an unmanaged custom enum type.
	UnmanagedEnum UnmanagedKind = "ENUM"
	// UnmanagedDomain indicates an unmanaged domain type.
	UnmanagedDomain UnmanagedKind = "DOMAIN"
)

// DependencyRef identifies a table or column that an unmanaged object depends on.
type DependencyRef struct {
	Table  string `json:"table"`
	Column string `json:"column,omitempty"`
}

// UnmanagedObject represents a database object detected in the live database that Grizzle does not manage.
type UnmanagedObject struct {
	Name      string          `json:"name"`
	Kind      UnmanagedKind   `json:"kind"`
	Table     string          `json:"table,omitempty"`
	DependsOn []DependencyRef `json:"depends_on,omitempty"`
}

// Table represents a table within a schema.
type Table struct {
	Name        string                 `json:"name"`
	Columns     map[string]*Column     `json:"columns"`
	Indexes     map[string]*Index      `json:"indexes"`
	ForeignKeys map[string]*ForeignKey `json:"foreign_keys"`
	PrimaryKey  *PrimaryKey            `json:"primary_key"`
}

// Column represents a single column within a table.
type Column struct {
	Name         string `json:"name"`
	DataType     string `json:"data_type"`
	IsNullable   bool   `json:"is_nullable"`
	DefaultValue string `json:"default_value"`
	Position     int    `json:"position"`
	IsIdentity   bool   `json:"is_identity,omitempty"`
	IdentityType string `json:"identity_type,omitempty"`
}

// Index represents a secondary or unique index on a table.
type Index struct {
	Name       string `json:"name"`
	TableName  string `json:"table_name"`
	IsUnique   bool   `json:"is_unique"`
	Definition string `json:"definition"`
	IsValid    bool   `json:"is_valid"`
}

// ForeignKey represents a foreign key constraint on a table.
type ForeignKey struct {
	Name       string `json:"name"`
	TableName  string `json:"table_name"`
	Definition string `json:"definition"`
	IsValid    bool   `json:"is_valid"`
}

// Enum represents a custom database ENUM type.
type Enum struct {
	Name   string   `json:"name"`
	Values []string `json:"values"`
}

// PrimaryKey represents the primary key constraint of a table.
type PrimaryKey struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
}
