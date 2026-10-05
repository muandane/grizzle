package schema

// Schema represents the parsed relational structure of a database schema.
type Schema struct {
	Name   string              `json:"name"`
	Tables map[string]*Table   `json:"tables"`
	Enums  map[string]*Enum    `json:"enums"`
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
}

// ForeignKey represents a foreign key constraint on a table.
type ForeignKey struct {
	Name       string `json:"name"`
	TableName  string `json:"table_name"`
	Definition string `json:"definition"`
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

// Legacy aliases for backward compatibility within Grizzle.
type (
	SchemaIR     = Schema
	TableIR      = Table
	ColumnIR     = Column
	IndexIR      = Index
	ForeignKeyIR = ForeignKey
	EnumIR       = Enum
	PrimaryKeyIR = PrimaryKey
)
