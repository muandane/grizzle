package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// ChangeType describes the category of a schema mutation.
type ChangeType string

// ChangeType constants define the supported kinds of atomic schema changes.
const (
	ChangeCreateEnum         ChangeType = "CREATE_ENUM"
	ChangeAlterEnum          ChangeType = "ALTER_ENUM"
	ChangeCreateTable        ChangeType = "CREATE_TABLE"
	ChangeDropTable          ChangeType = "DROP_TABLE"
	ChangeAddColumn          ChangeType = "ADD_COLUMN"
	ChangeDropColumn         ChangeType = "DROP_COLUMN"
	ChangeAlterColumn        ChangeType = "ALTER_COLUMN"
	ChangeRenameColumn       ChangeType = "RENAME_COLUMN"
	ChangeCreateIndex        ChangeType = "CREATE_INDEX"
	ChangeDropIndex          ChangeType = "DROP_INDEX"
	ChangeAddFK              ChangeType = "ADD_FK"
	ChangeDropFK             ChangeType = "DROP_FK"
	ChangeAddCheck           ChangeType = "ADD_CHECK"
	ChangeDropCheck          ChangeType = "DROP_CHECK"
	ChangeValidateConstraint ChangeType = "VALIDATE_CONSTRAINT"
	ChangeAttachPartition    ChangeType = "ATTACH_PARTITION"
	ChangeDetachPartition    ChangeType = "DETACH_PARTITION"
	ChangeCreateExtension    ChangeType = "CREATE_EXTENSION"
	ChangeDropExtension      ChangeType = "DROP_EXTENSION"
	ChangeEnableRLS          ChangeType = "ENABLE_RLS"
	ChangeDisableRLS         ChangeType = "DISABLE_RLS"
	ChangeForceRLS           ChangeType = "FORCE_RLS"
	ChangeNoForceRLS         ChangeType = "NO_FORCE_RLS"
	ChangeCreatePolicy       ChangeType = "CREATE_POLICY"
	ChangeDropPolicy         ChangeType = "DROP_POLICY"
	ChangeCreateFunction     ChangeType = "CREATE_FUNCTION"
	ChangeDropFunction       ChangeType = "DROP_FUNCTION"
	ChangeCreateTrigger      ChangeType = "CREATE_TRIGGER"
	ChangeDropTrigger        ChangeType = "DROP_TRIGGER"
	ChangeCreateView         ChangeType = "CREATE_VIEW"
	ChangeDropView           ChangeType = "DROP_VIEW"
	ChangeRefreshMatView     ChangeType = "REFRESH_MATVIEW"
)

// Step represents a single atomic DDL migration statement.
type Step struct {
	Type        ChangeType `json:"type"`
	Table       string     `json:"table"`
	Column      string     `json:"column,omitzero"`
	SQL         string     `json:"sql"`
	Destructive bool       `json:"destructive,omitzero"`
	NonTx       bool       `json:"non_tx,omitzero"`

	// Structural column metadata for precise hazard analysis
	ColumnNotNull      bool     `json:"column_not_null,omitzero"`
	ColumnHasDefault   bool     `json:"column_has_default,omitzero"`
	TypeNarrowed       bool     `json:"type_narrowed,omitzero"`
	IsTableRebuild     bool     `json:"is_table_rebuild,omitzero"`
	IsRenameCandidate  bool     `json:"is_rename_candidate,omitzero"`
	IsGeneratedRewrite bool     `json:"is_generated_rewrite,omitzero"`
	OldColumn          string   `json:"old_column,omitzero"`
	UnmanagedDeps      []string `json:"unmanaged_deps,omitzero"`

	// Partitioning metadata
	ParentTable     string `json:"parent_table,omitzero"`
	PartitionBounds string `json:"partition_bounds,omitzero"`
	IsPendingDetach bool   `json:"is_pending_detach,omitzero"`

	// Relational dependency metadata for multi-schema toposort
	Schema    string   `json:"schema,omitzero"`
	DependsOn []string `json:"depends_on,omitzero"`
	RefTable  string   `json:"ref_table,omitzero"`

	// ValidatesCheck marks a VALIDATE_CONSTRAINT step that validates a CHECK
	// constraint (as opposed to a foreign key). Used for hazard classification
	// (CHECK validate runs a full table scan under SHARE UPDATE EXCLUSIVE).
	ValidatesCheck bool `json:"validates_check,omitzero"`

	// Replace marks a CREATE_VIEW step rendered as CREATE OR REPLACE VIEW.
	Replace bool `json:"replace,omitzero"`
}

// DropPolicy defines fine-grained permissions for destructive operations.
type DropPolicy struct {
	AllowTable     bool `json:"allow_table"`
	AllowColumn    bool `json:"allow_column"`
	AllowIndex     bool `json:"allow_index"`
	AllowFK        bool `json:"allow_fk"`
	AllowCheck     bool `json:"allow_check"`
	AllowExtension bool `json:"allow_extension"`
	AllowFunction  bool `json:"allow_function"`
	AllowPolicy    bool `json:"allow_policy"`
	AllowTrigger   bool `json:"allow_trigger"`
	AllowView      bool `json:"allow_view"`
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
	case ChangeDropCheck:
		return p.AllowCheck
	case ChangeAlterColumn:
		return p.AllowColumn
	case ChangeDropExtension:
		return p.AllowExtension
	case ChangeDropFunction:
		return p.AllowFunction
	case ChangeDropPolicy:
		return p.AllowPolicy
	case ChangeDropTrigger:
		return p.AllowTrigger
	case ChangeDropView:
		return p.AllowView
	default:
		return false
	}
}

// Plan contains the complete list of sequenced migration steps.
type Plan struct {
	TargetSchema   string            `json:"target_schema"`
	TargetSchemas  []string          `json:"target_schemas,omitempty"`
	Steps          []Step            `json:"steps"`
	Policy         DropPolicy        `json:"policy"`
	IncludeTables  []string          `json:"include_tables,omitzero"`
	ExcludeTables  []string          `json:"exclude_tables,omitzero"`
	Renames        map[string]string `json:"renames,omitzero"`
	ExpandContract bool              `json:"expand_contract,omitzero"`
	SchemaSQL      string            `json:"schema_sql,omitzero"`

	// NonConcurrentIndexes affects generated SQL (CONCURRENTLY vs
	// transactional CREATE INDEX) and is therefore approval-sensitive:
	// it participates in Hash().
	NonConcurrentIndexes bool `json:"non_concurrent_indexes,omitempty"`

	// Operational timeouts persisted so direct apply can reproduce the
	// timeout posture used at plan time. Bounded by ValidateExecutionFields;
	// they do NOT participate in Hash() because they cannot change executed
	// SQL or destructive policy. Lock identity and shadow schema names are
	// never persisted: LockID is derived at apply from trusted target
	// identity, LockNamespace is an ApplyOpts runtime option, and shadow
	// schemas are generated ephemerally at apply time.
	LockTimeout      time.Duration `json:"lock_timeout_ns,omitempty"`
	StatementTimeout time.Duration `json:"statement_timeout_ns,omitempty"`
}

// Hash computes a deterministic SHA-256 hex digest of the canonical step list and scope.
func (p *Plan) Hash() string {
	h := sha256.New()

	includes := slices.Clone(p.IncludeTables)
	slices.Sort(includes)
	excludes := slices.Clone(p.ExcludeTables)
	slices.Sort(excludes)

	write := func(format string, args ...any) {
		_, _ = fmt.Fprintf(h, format, args...)
	}

	schema := p.TargetSchema
	if len(p.TargetSchemas) > 1 {
		schemas := slices.Clone(p.TargetSchemas)
		slices.Sort(schemas)
		schemas = slices.Compact(schemas)
		schema = schemas[0]
		write("schema:%s\n", schema)
		write("schemas:%s\n", strings.Join(schemas, ","))
	} else {
		write("schema:%s\n", schema)
	}
	write("includes:%s\n", strings.Join(includes, ","))
	write("excludes:%s\n", strings.Join(excludes, ","))

	if len(p.Renames) > 0 {
		renameKeys := slices.Collect(maps.Keys(p.Renames))
		slices.Sort(renameKeys)
		for _, k := range renameKeys {
			write("rename:%s->%s\n", k, p.Renames[k])
		}
	}
	if p.ExpandContract {
		write("expand_contract:true\n")
	}
	write("policy:%t,%t,%t,%t,%t,%t,%t,%t,%t,%t\n",
		p.Policy.AllowTable, p.Policy.AllowColumn, p.Policy.AllowIndex,
		p.Policy.AllowFK, p.Policy.AllowCheck,
		p.Policy.AllowExtension, p.Policy.AllowFunction, p.Policy.AllowPolicy,
		p.Policy.AllowTrigger, p.Policy.AllowView)
	if p.NonConcurrentIndexes {
		write("non_concurrent:true\n")
	}
	if p.SchemaSQL != "" {
		write("schema_sql:%s\n", p.SchemaSQL)
	}

	for i, s := range p.Steps {
		unmStr := ""
		if len(s.UnmanagedDeps) > 0 {
			unmStr = "|unmanaged_deps:" + strings.Join(s.UnmanagedDeps, ",")
		}
		partStr := ""
		if s.ParentTable != "" {
			partStr = "|parent:" + s.ParentTable
		}
		if s.PartitionBounds != "" {
			partStr += "|bounds:" + s.PartitionBounds
		}
		schemaStr := ""
		if s.Schema != "" {
			schemaStr = "|schema:" + s.Schema
		}
		pendingStr := ""
		if s.IsPendingDetach {
			pendingStr = "|pending_detach:true"
		}
		write("step:%d|type:%s|table:%s|sql:%s|destructive:%t|non_tx:%t|not_null:%t|default:%t|narrowed:%t|rebuild:%t|rename_cand:%t|gen_rewrite:%t|old_col:%s%s%s%s%s\n",
			i, s.Type, s.Table, strings.TrimSpace(s.SQL), s.Destructive, s.NonTx,
			s.ColumnNotNull, s.ColumnHasDefault, s.TypeNarrowed, s.IsTableRebuild,
			s.IsRenameCandidate, s.IsGeneratedRewrite, s.OldColumn, unmStr, partStr, schemaStr, pendingStr,
		)
	}

	return hex.EncodeToString(h.Sum(nil))
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
		case ChangeCreateEnum, ChangeCreateTable, ChangeAddColumn, ChangeCreateIndex, ChangeAddFK, ChangeAddCheck,
			ChangeCreateExtension, ChangeCreatePolicy, ChangeCreateFunction, ChangeCreateTrigger, ChangeCreateView,
			ChangeEnableRLS, ChangeForceRLS:
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
		case ChangeAlterColumn, ChangeAlterEnum, ChangeRefreshMatView,
			ChangeDisableRLS, ChangeNoForceRLS:
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
		case ChangeDropTable, ChangeDropColumn, ChangeDropIndex, ChangeDropFK, ChangeDropCheck,
			ChangeDropExtension, ChangeDropPolicy, ChangeDropFunction, ChangeDropTrigger, ChangeDropView:
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

// HazardCode represents a stable, machine-readable identifier for a migration hazard.
type HazardCode string

const (
	// HazardDropTable indicates a table drop resulting in complete data loss.
	HazardDropTable HazardCode = "DROP_TABLE"
	// HazardDropColumn indicates a column drop resulting in data loss for row values.
	HazardDropColumn HazardCode = "DROP_COLUMN"
	// HazardTypeNarrow indicates a column type change that narrows capacity or causes data loss/truncation.
	HazardTypeNarrow HazardCode = "TYPE_NARROW"
	// HazardNotNullNoDefault indicates adding a NOT NULL column without a default to an existing table.
	HazardNotNullNoDefault HazardCode = "NOT_NULL_NO_DEFAULT"
	// HazardIndexBuild indicates index creation table locking or execution load.
	HazardIndexBuild HazardCode = "INDEX_BUILD"
	// HazardDropIndex indicates query performance degradation risk from dropping an index.
	HazardDropIndex HazardCode = "DROP_INDEX"
	// HazardDropFK indicates removing referential integrity enforcement.
	HazardDropFK HazardCode = "DROP_FK"
	// HazardDropCheck indicates removing row-validation enforcement from a table.
	HazardDropCheck HazardCode = "DROP_CHECK"
	// HazardCheckValidateScan indicates a VALIDATE CONSTRAINT table scan under
	// SHARE UPDATE EXCLUSIVE lock for a newly staged CHECK constraint.
	HazardCheckValidateScan HazardCode = "CHECK_VALIDATE_SCAN"
	// HazardRenameAmbiguous indicates an ambiguous column rename candidate (same type dropped and added).
	HazardRenameAmbiguous HazardCode = "RENAME_AMBIGUOUS"
	// HazardUnmanagedDependency indicates a drop or type change touches a column/table that an unmanaged object depends on.
	HazardUnmanagedDependency HazardCode = "UNMANAGED_DEPENDENCY"
	// HazardGeneratedRewrite indicates changing a generated column expression requiring table rewrite.
	HazardGeneratedRewrite HazardCode = "GENERATED_REWRITE"
	// HazardPartitionAttachScan indicates attaching an existing table to a partitioned table requiring table scan.
	HazardPartitionAttachScan HazardCode = "PARTITION_ATTACH_SCAN"
	// HazardPartitionPendingDetach indicates an interrupted pending-detach state requiring finalization.
	HazardPartitionPendingDetach HazardCode = "PARTITION_PENDING_DETACH"
	// HazardExtensionPrivilege indicates CREATE EXTENSION may require elevated privileges.
	HazardExtensionPrivilege HazardCode = "EXTENSION_PRIVILEGE"
	// HazardDropExtension indicates dropping a PostgreSQL extension.
	HazardDropExtension HazardCode = "DROP_EXTENSION"
	// HazardDropPolicy indicates dropping a row-level security policy.
	HazardDropPolicy HazardCode = "DROP_POLICY"
	// HazardRLSEnable indicates enabling RLS may lock out roles without matching policies.
	HazardRLSEnable HazardCode = "RLS_ENABLE"
	// HazardDropFunction indicates dropping a function or procedure.
	HazardDropFunction HazardCode = "DROP_FUNCTION"
	// HazardSecurityDefiner indicates a SECURITY DEFINER routine without an explicit search_path.
	HazardSecurityDefiner HazardCode = "SECURITY_DEFINER"
	// HazardDropTrigger indicates dropping a trigger.
	HazardDropTrigger HazardCode = "DROP_TRIGGER"
	// HazardDropView indicates dropping a view or materialized view.
	HazardDropView HazardCode = "DROP_VIEW"
)

// HazardLevel indicates the operational or data-loss severity of a migration step.
type HazardLevel string

const (
	// HazardLevelCritical indicates potential data loss or immediate runtime failure (e.g. DROP TABLE, DROP COLUMN, TYPE_NARROW, NOT_NULL_NO_DEFAULT).
	HazardLevelCritical HazardLevel = "CRITICAL"
	// HazardLevelWarning indicates operational execution risk.
	HazardLevelWarning HazardLevel = "WARNING"
	// HazardLevelNotice indicates table locking or performance implications (e.g. index build).
	HazardLevelNotice HazardLevel = "NOTICE"
)

// Hazard describes an operational risk detected in a planned migration step.
type Hazard struct {
	Code        HazardCode  `json:"code"`
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
		hazards = append(hazards, stepHazards(s)...)
	}
	return hazards
}

// stepHazards analyzes an individual planned step and returns detected operational and data-loss risks.
func stepHazards(s Step) []Hazard {
	var hazards []Hazard
	if len(s.UnmanagedDeps) > 0 {
		hazards = append(hazards, Hazard{
			Code:  HazardUnmanagedDependency,
			Level: HazardLevelCritical,
			Type:  s.Type,
			Table: s.Table,
			Description: fmt.Sprintf(
				"Table %q operation affects unmanaged dependent object(s) [%s]; Grizzle does not manage these objects, so the operation can leave them broken. Remediation: accept with AcceptHazards: UNMANAGED_DEPENDENCY and drop/recreate the unmanaged object(s) outside Grizzle, or remove this operation",
				s.Table, strings.Join(s.UnmanagedDeps, ", "),
			),
			SQL: s.SQL,
		})
	}
	if s.IsGeneratedRewrite {
		// CRITICAL: generated-column rewrites are rendered as DROP COLUMN +
		// ADD COLUMN (PostgreSQL) or full table rebuild (SQLite), so they are
		// materially destructive and must be explicitly accepted.
		hazards = append(hazards, Hazard{
			Code:        HazardGeneratedRewrite,
			Level:       HazardLevelCritical,
			Type:        s.Type,
			Table:       s.Table,
			Description: fmt.Sprintf("Generated column on table %q expression modified; requires destructive rewrite (drop/recreate column or table rebuild)", s.Table),
			SQL:         s.SQL,
		})
	}
	switch s.Type {
	case ChangeDetachPartition:
		if s.IsPendingDetach {
			hazards = append(hazards, Hazard{
				Code:        HazardPartitionPendingDetach,
				Level:       HazardLevelWarning,
				Type:        s.Type,
				Table:       s.Table,
				Description: fmt.Sprintf("Partition %q is in an interrupted pending-detach state; finalizing detachment", s.Table),
				SQL:         s.SQL,
			})
		}
	case ChangeAttachPartition:
		hazards = append(hazards, Hazard{
			Code:        HazardPartitionAttachScan,
			Level:       HazardLevelWarning,
			Type:        s.Type,
			Table:       s.Table,
			Description: fmt.Sprintf("Attaching existing table %q to parent %q requires a table validation scan under ACCESS EXCLUSIVE lock", s.Table, s.ParentTable),
			SQL:         s.SQL,
		})
	case ChangeDropTable:
		hazards = append(hazards, Hazard{
			Code:        HazardDropTable,
			Level:       HazardLevelCritical,
			Type:        s.Type,
			Table:       s.Table,
			Description: fmt.Sprintf("Table %q will be dropped with all its data and dependent objects", s.Table),
			SQL:         s.SQL,
		})
	case ChangeDropColumn:
		desc := fmt.Sprintf("Column on table %q will be dropped with all existing row values", s.Table)
		if s.IsTableRebuild {
			// SQLite rebuild path: whole table is dropped and recreated.
			desc = fmt.Sprintf("Table %q will be dropped and recreated; only matching columns are copied back", s.Table)
		}
		hazards = append(hazards, Hazard{
			Code:        HazardDropColumn,
			Level:       HazardLevelCritical,
			Type:        s.Type,
			Table:       s.Table,
			Description: desc,
			SQL:         s.SQL,
		})
		if s.IsRenameCandidate {
			hazards = append(hazards, Hazard{
				Code:        HazardRenameAmbiguous,
				Level:       HazardLevelCritical,
				Type:        s.Type,
				Table:       s.Table,
				Description: fmt.Sprintf("Table %q has ambiguous column rename candidate for dropped column %q; requires explicit mapping in Options.Renames or separate plans", s.Table, s.OldColumn),
				SQL:         s.SQL,
			})
		}
	case ChangeAlterColumn:
		if s.TypeNarrowed {
			hazards = append(hazards, Hazard{
				Code:        HazardTypeNarrow,
				Level:       HazardLevelCritical,
				Type:        s.Type,
				Table:       s.Table,
				Description: fmt.Sprintf("Column on table %q has a destructive type change that may cause data loss or truncation", s.Table),
				SQL:         s.SQL,
			})
		} else if !s.IsGeneratedRewrite {
			hazards = append(hazards, Hazard{
				Code:        "ALTER_COLUMN",
				Level:       HazardLevelNotice,
				Type:        s.Type,
				Table:       s.Table,
				Description: fmt.Sprintf("Column on table %q will be modified", s.Table),
				SQL:         s.SQL,
			})
		}
	case ChangeAddColumn:
		if s.ColumnNotNull && !s.ColumnHasDefault {
			hazards = append(hazards, Hazard{
				Code:        HazardNotNullNoDefault,
				Level:       HazardLevelCritical,
				Type:        s.Type,
				Table:       s.Table,
				Description: fmt.Sprintf("Adding NOT NULL column without DEFAULT to existing table %q will fail if the table contains rows", s.Table),
				SQL:         s.SQL,
			})
		}
	case ChangeCreateIndex:
		hazards = append(hazards, Hazard{
			Code:        HazardIndexBuild,
			Level:       HazardLevelNotice,
			Type:        s.Type,
			Table:       s.Table,
			Description: fmt.Sprintf("Index creation on table %q acquires a ShareLock unless created concurrently", s.Table),
			SQL:         s.SQL,
		})
	case ChangeDropIndex:
		hazards = append(hazards, Hazard{
			Code:        HazardDropIndex,
			Level:       HazardLevelNotice,
			Type:        s.Type,
			Table:       s.Table,
			Description: fmt.Sprintf("Dropping index on table %q may degrade active query performance", s.Table),
			SQL:         s.SQL,
		})
	case ChangeDropFK:
		hazards = append(hazards, Hazard{
			Code:        HazardDropFK,
			Level:       HazardLevelNotice,
			Type:        s.Type,
			Table:       s.Table,
			Description: fmt.Sprintf("Dropping foreign key constraint on table %q removes referential integrity enforcement", s.Table),
			SQL:         s.SQL,
		})
	case ChangeDropCheck:
		hazards = append(hazards, Hazard{
			Code:        HazardDropCheck,
			Level:       HazardLevelNotice,
			Type:        s.Type,
			Table:       s.Table,
			Description: fmt.Sprintf("Dropping check constraint on table %q removes row-validation enforcement", s.Table),
			SQL:         s.SQL,
		})
	case ChangeValidateConstraint:
		if s.ValidatesCheck {
			hazards = append(hazards, Hazard{
				Code:        HazardCheckValidateScan,
				Level:       HazardLevelNotice,
				Type:        s.Type,
				Table:       s.Table,
				Description: fmt.Sprintf("Validating check constraint on table %q runs a full table scan under SHARE UPDATE EXCLUSIVE lock", s.Table),
				SQL:         s.SQL,
			})
		}
	case ChangeCreateExtension:
		hazards = append(hazards, Hazard{
			Code:        HazardExtensionPrivilege,
			Level:       HazardLevelWarning,
			Type:        s.Type,
			Table:       s.Table,
			Description: fmt.Sprintf("Creating extension %q may require superuser or CREATE privilege on the database", s.Table),
			SQL:         s.SQL,
		})
	case ChangeDropExtension:
		hazards = append(hazards, Hazard{
			Code:        HazardDropExtension,
			Level:       HazardLevelCritical,
			Type:        s.Type,
			Table:       s.Table,
			Description: fmt.Sprintf("Extension %q will be dropped along with objects it owns", s.Table),
			SQL:         s.SQL,
		})
	case ChangeEnableRLS, ChangeForceRLS:
		hazards = append(hazards, Hazard{
			Code:        HazardRLSEnable,
			Level:       HazardLevelWarning,
			Type:        s.Type,
			Table:       s.Table,
			Description: fmt.Sprintf("Enabling RLS on table %q can lock out roles that lack matching policies", s.Table),
			SQL:         s.SQL,
		})
	case ChangeDropPolicy:
		hazards = append(hazards, Hazard{
			Code:        HazardDropPolicy,
			Level:       HazardLevelCritical,
			Type:        s.Type,
			Table:       s.Table,
			Description: fmt.Sprintf("Row-level security policy on table %q will be dropped", s.Table),
			SQL:         s.SQL,
		})
	case ChangeDropFunction:
		hazards = append(hazards, Hazard{
			Code:        HazardDropFunction,
			Level:       HazardLevelCritical,
			Type:        s.Type,
			Table:       s.Table,
			Description: fmt.Sprintf("Function/procedure %q will be dropped", s.Table),
			SQL:         s.SQL,
		})
	case ChangeCreateFunction:
		if strings.Contains(strings.ToUpper(s.SQL), "SECURITY DEFINER") &&
			!strings.Contains(strings.ToLower(s.SQL), "search_path") {
			hazards = append(hazards, Hazard{
				Code:        HazardSecurityDefiner,
				Level:       HazardLevelWarning,
				Type:        s.Type,
				Table:       s.Table,
				Description: fmt.Sprintf("SECURITY DEFINER routine %q has no explicit search_path; set search_path to avoid privilege escalation via schema shadowing", s.Table),
				SQL:         s.SQL,
			})
		}
	case ChangeDropTrigger:
		hazards = append(hazards, Hazard{
			Code:        HazardDropTrigger,
			Level:       HazardLevelCritical,
			Type:        s.Type,
			Table:       s.Table,
			Description: fmt.Sprintf("Trigger on table %q will be dropped", s.Table),
			SQL:         s.SQL,
		})
	case ChangeDropView:
		hazards = append(hazards, Hazard{
			Code:        HazardDropView,
			Level:       HazardLevelCritical,
			Type:        s.Type,
			Table:       s.Table,
			Description: fmt.Sprintf("View %q will be dropped", s.Table),
			SQL:         s.SQL,
		})
	}
	return hazards
}

// ValidateHazards checks whether any critical hazards in the plan are not explicitly accepted.
func (p *Plan) ValidateHazards(accept []HazardCode) error {
	var unaccepted []Hazard
	for _, h := range p.Hazards() {
		if h.Level == HazardLevelCritical {
			if !slices.Contains(accept, h.Code) {
				unaccepted = append(unaccepted, h)
			}
		}
	}
	if len(unaccepted) > 0 {
		return &HazardError{Hazards: unaccepted}
	}
	return nil
}

// ValidatePolicy checks whether all planned migration steps comply with the configured drop safety policy.
func (p *Plan) ValidatePolicy() error {
	var violations []Step
	for _, s := range p.Steps {
		if !p.Policy.IsAllowed(s) {
			violations = append(violations, s)
		}
	}
	if len(violations) > 0 {
		return &DestructiveViolationError{Violations: violations}
	}
	return nil
}
