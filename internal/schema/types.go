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
	SQL       string          `json:"sql,omitempty"`
}

// PartitionStrategy defines the table partitioning method.
type PartitionStrategy string

const (
	// PartitionStrategyRange defines range-based partitioning.
	PartitionStrategyRange PartitionStrategy = "RANGE"
	// PartitionStrategyList defines list-based partitioning.
	PartitionStrategyList PartitionStrategy = "LIST"
	// PartitionStrategyHash defines hash-based partitioning.
	PartitionStrategyHash PartitionStrategy = "HASH"
)

// PartitionKey defines partition method and key definition for a partitioned table.
type PartitionKey struct {
	Strategy PartitionStrategy `json:"strategy"`
	Def      string            `json:"def"` // e.g. "RANGE (log_date)" or "RANGE (city_id, log_date)"
}

// PartitionOf defines attachment parameters for a partition table to its parent table.
type PartitionOf struct {
	Parent string `json:"parent"` // Name of parent partitioned table
	Bounds string `json:"bounds"` // e.g. "FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')"
}

// Table represents a table within a schema.
type Table struct {
	Schema       string                 `json:"schema,omitempty"`
	Name         string                 `json:"name"`
	Columns      map[string]*Column     `json:"columns"`
	Indexes      map[string]*Index      `json:"indexes"`
	ForeignKeys  map[string]*ForeignKey `json:"foreign_keys"`
	PrimaryKey   *PrimaryKey            `json:"primary_key"`
	PartitionKey *PartitionKey          `json:"partition_key,omitempty"`
	PartitionOf  *PartitionOf           `json:"partition_of,omitempty"`
}

// IsPartitioned returns true if the table is a partitioned table.
func (t *Table) IsPartitioned() bool {
	return t != nil && t.PartitionKey != nil
}

// IsPartition returns true if the table is a partition of another table.
func (t *Table) IsPartition() bool {
	return t != nil && t.PartitionOf != nil
}

// GeneratedColumn describes a computed or generated column.
type GeneratedColumn struct {
	Expr   string `json:"expr"`
	Stored bool   `json:"stored"`
}

// Column represents a single column within a table.
type Column struct {
	Name         string           `json:"name"`
	DataType     string           `json:"data_type"`
	IsNullable   bool             `json:"is_nullable"`
	DefaultValue string           `json:"default_value"`
	Position     int              `json:"position"`
	IsIdentity   bool             `json:"is_identity,omitempty"`
	IdentityType string           `json:"identity_type,omitempty"`
	Generated    *GeneratedColumn `json:"generated,omitempty"`
}

// Index represents a secondary or unique index on a table.
type Index struct {
	Name       string `json:"name"`
	TableName  string `json:"table_name"`
	IsUnique   bool   `json:"is_unique"`
	Definition string `json:"definition"`
	IsValid    bool   `json:"is_valid"`
	Predicate  string `json:"predicate,omitempty"`
}

// IsPartial reports whether the index contains a WHERE predicate filter.
func (idx *Index) IsPartial() bool {
	return idx != nil && idx.Predicate != ""
}

// ForeignKey represents a foreign key constraint on a table.
type ForeignKey struct {
	Name       string `json:"name"`
	TableName  string `json:"table_name"`
	RefSchema  string `json:"ref_schema,omitempty"`
	RefTable   string `json:"ref_table,omitempty"`
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
