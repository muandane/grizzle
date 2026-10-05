package grizzle

// Options configures the schema synchronization process.
type Options struct {
	// SchemaSQL is the complete DDL representing the desired database state.
	// Typically provided via //go:embed schema.sql.
	SchemaSQL string

	// TargetSchema is the database schema to manage (defaults to "public").
	TargetSchema string

	// ShadowSchema is the temporary schema used to compile and validate SchemaSQL.
	// Defaults to "_grizzle_shadow".
	ShadowSchema string

	// AllowDrop permits destructive operations such as DROP TABLE, DROP COLUMN, DROP INDEX, and DROP CONSTRAINT.
	// Defaults to false to prevent accidental data loss in production.
	AllowDrop bool

	// LockID is a 64-bit integer used for the PostgreSQL advisory lock (pg_advisory_xact_lock).
	// If 0, Grizzle derives a deterministic lock ID from the TargetSchema.
	LockID int64
}

// ChangeType represents the kind of DDL operation planned or applied.
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
}

// Plan contains the complete list of sequenced migration steps.
type Plan struct {
	TargetSchema string `json:"target_schema"`
	Steps        []Step `json:"steps"`
}

// HasDestructive reports whether any step in the plan is destructive (e.g. DROP).
func (p *Plan) HasDestructive() bool {
	for _, s := range p.Steps {
		if s.Destructive {
			return true
		}
	}
	return false
}

// --- Internal Schema Representation (IR) ---

// SchemaIR represents the parsed relational structure of a database schema.
type SchemaIR struct {
	Name   string
	Tables map[string]*TableIR
	Enums  map[string]*EnumIR
}

// TableIR represents a table within a schema.
type TableIR struct {
	Name        string
	Columns     map[string]*ColumnIR
	Indexes     map[string]*IndexIR
	ForeignKeys map[string]*ForeignKeyIR
	PrimaryKey  *PrimaryKeyIR
}

// ColumnIR represents a single column within a table.
type ColumnIR struct {
	Name         string
	DataType     string // Normalized PostgreSQL type (e.g. "bigint", "varchar(255)", "boolean")
	IsNullable   bool
	DefaultValue string // Sanitized default expression (without Postgres explicit type casts)
	Position     int
	IsIdentity   bool
	IdentityType string // "ALWAYS" or "BY DEFAULT"
}

// IndexIR represents a secondary or unique index on a table.
type IndexIR struct {
	Name       string
	TableName  string
	IsUnique   bool
	Definition string // Normalized index DDL (e.g. CREATE [UNIQUE] INDEX ... ON ...)
}

// ForeignKeyIR represents a foreign key constraint on a table.
type ForeignKeyIR struct {
	Name       string
	TableName  string
	Definition string // Normalized constraint definition (e.g. FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE)
}

// EnumIR represents a custom PostgreSQL ENUM type.
type EnumIR struct {
	Name   string
	Values []string
}

// PrimaryKeyIR represents the primary key constraint of a table.
type PrimaryKeyIR struct {
	Name    string
	Columns []string
}
