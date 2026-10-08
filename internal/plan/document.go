package plan

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/muandane/grizzle/internal/schema"
)

// ScopeDocument records included and excluded table patterns for the plan.
type ScopeDocument struct {
	Includes []string `json:"includes,omitzero"`
	Excludes []string `json:"excludes,omitzero"`
}

// Document represents a complete, serialized migration plan artifact including hash, hazards, and scope.
type Document struct {
	Hash           string            `json:"hash"`
	TargetSchema   string            `json:"target_schema"`
	TargetSchemas  []string          `json:"target_schemas,omitzero"`
	Scope          ScopeDocument     `json:"scope"`
	Hazards        []Hazard          `json:"hazards"`
	Steps          []Step            `json:"steps"`
	Policy         DropPolicy        `json:"policy"`
	Renames        map[string]string `json:"renames,omitzero"`
	ExpandContract bool              `json:"expand_contract,omitzero"`
	SchemaSQL      string            `json:"schema_sql,omitzero"`
	RolesSQL       string            `json:"roles_sql,omitzero"`
	CatalogSQL     string            `json:"catalog_sql,omitzero"`

	// Approval-sensitive execution semantics that affect generated SQL.
	NonConcurrentIndexes bool `json:"non_concurrent_indexes,omitempty"`

	// Operational timeouts persisted so direct apply reproduces the timeout
	// posture used to generate the plan. Durations are serialized as
	// nanoseconds. They do not participate in Hash(). Lock identity and
	// shadow schema names are never persisted in the artifact.
	LockTimeoutNs      int64 `json:"lock_timeout_ns,omitempty"`
	StatementTimeoutNs int64 `json:"statement_timeout_ns,omitempty"`
}

// Document converts the plan into a complete Document with recomputed hash and hazards.
func (p *Plan) Document() Document {
	includes := slices.Clone(p.IncludeTables)
	slices.Sort(includes)
	excludes := slices.Clone(p.ExcludeTables)
	slices.Sort(excludes)

	return Document{
		Hash:          p.Hash(),
		TargetSchema:  p.TargetSchema,
		TargetSchemas: p.TargetSchemas,
		Scope: ScopeDocument{
			Includes: includes,
			Excludes: excludes,
		},
		Hazards:              p.Hazards(),
		Steps:                p.Steps,
		Policy:               p.Policy,
		Renames:              p.Renames,
		ExpandContract:       p.ExpandContract,
		SchemaSQL:            schema.RedactSecretsSQL(p.SchemaSQL),
		RolesSQL:             schema.RedactRolePasswordsSQL(p.RolesSQL),
		CatalogSQL:           schema.RedactSubscriptionConnInfoSQL(p.CatalogSQL),
		NonConcurrentIndexes: p.NonConcurrentIndexes,
		LockTimeoutNs:        int64(p.LockTimeout / time.Nanosecond),
		StatementTimeoutNs:   int64(p.StatementTimeout / time.Nanosecond),
	}
}

// ToJSON returns canonical formatted JSON for the plan document.
func (p *Plan) ToJSON() ([]byte, error) {
	doc := p.Document()
	return json.MarshalIndent(doc, "", "  ")
}

// ParsePlanJSON parses a plan document from JSON bytes, supporting both Document envelope and plain Plan.
func ParsePlanJSON(data []byte) (*Plan, string, error) {
	var doc Document
	if err := json.Unmarshal(data, &doc); err == nil && doc.Hash != "" {
		p := &Plan{
			TargetSchema:         doc.TargetSchema,
			TargetSchemas:        doc.TargetSchemas,
			Steps:                doc.Steps,
			Policy:               doc.Policy,
			IncludeTables:        doc.Scope.Includes,
			ExcludeTables:        doc.Scope.Excludes,
			Renames:              doc.Renames,
			ExpandContract:       doc.ExpandContract,
			SchemaSQL:            doc.SchemaSQL,
			RolesSQL:             doc.RolesSQL,
			CatalogSQL:           doc.CatalogSQL,
			LockTimeout:          time.Duration(doc.LockTimeoutNs),
			StatementTimeout:     time.Duration(doc.StatementTimeoutNs),
			NonConcurrentIndexes: doc.NonConcurrentIndexes,
		}
		if err := p.ValidateExecutionFields(); err != nil {
			return nil, "", err
		}
		return p, doc.Hash, nil
	}

	var p Plan
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, "", fmt.Errorf("grizzle: failed parsing plan JSON: %w", err)
	}
	if err := p.ValidateExecutionFields(); err != nil {
		return nil, "", err
	}
	return &p, p.Hash(), nil
}
