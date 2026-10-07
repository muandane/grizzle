package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// ScopeDocument records included and excluded table patterns for the plan.
type ScopeDocument struct {
	Includes []string `json:"includes,omitzero"`
	Excludes []string `json:"excludes,omitzero"`
}

// Document represents a complete, serialized migration plan artifact including hash, hazards, scope, and options digest.
type Document struct {
	Hash           string            `json:"hash"`
	TargetSchema   string            `json:"target_schema"`
	TargetSchemas  []string          `json:"target_schemas,omitzero"`
	Scope          ScopeDocument     `json:"scope"`
	OptionsDigest  string            `json:"options_digest"`
	Hazards        []Hazard          `json:"hazards"`
	Steps          []Step            `json:"steps"`
	Policy         DropPolicy        `json:"policy"`
	Renames        map[string]string `json:"renames,omitzero"`
	ExpandContract bool              `json:"expand_contract,omitzero"`
	SchemaSQL      string            `json:"schema_sql,omitzero"`

	// Operational knobs persisted so direct apply reproduces the locking and
	// timeout posture used to generate the plan. Durations are serialized as
	// nanoseconds. They do not participate in Hash().
	LockID               int64  `json:"lock_id,omitempty"`
	LockNamespace        string `json:"lock_namespace,omitempty"`
	LockTimeoutNs        int64  `json:"lock_timeout_ns,omitempty"`
	StatementTimeoutNs   int64  `json:"statement_timeout_ns,omitempty"`
	NonConcurrentIndexes bool   `json:"non_concurrent_indexes,omitempty"`
	ShadowSchema         string `json:"shadow_schema,omitempty"`
}

// Document converts the plan into a complete Document with recomputed hash and hazards.
func (p *Plan) Document() Document {
	includes := slices.Clone(p.IncludeTables)
	slices.Sort(includes)
	excludes := slices.Clone(p.ExcludeTables)
	slices.Sort(excludes)

	h := sha256.New()
	_, _ = fmt.Fprintf(h, "schema:%s|steps:%d|renames:%d|expand:%t", p.TargetSchema, len(p.Steps), len(p.Renames), p.ExpandContract)
	optDigest := hex.EncodeToString(h.Sum(nil))

	return Document{
		Hash:          p.Hash(),
		TargetSchema:  p.TargetSchema,
		TargetSchemas: p.TargetSchemas,
		Scope: ScopeDocument{
			Includes: includes,
			Excludes: excludes,
		},
		OptionsDigest:        optDigest,
		Hazards:              p.Hazards(),
		Steps:                p.Steps,
		Policy:               p.Policy,
		Renames:              p.Renames,
		ExpandContract:       p.ExpandContract,
		SchemaSQL:            p.SchemaSQL,
		LockID:               p.LockID,
		LockNamespace:        p.LockNamespace,
		LockTimeoutNs:        int64(p.LockTimeout / time.Nanosecond),
		StatementTimeoutNs:   int64(p.StatementTimeout / time.Nanosecond),
		NonConcurrentIndexes: p.NonConcurrentIndexes,
		ShadowSchema:         p.ShadowSchema,
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
			LockID:               doc.LockID,
			LockNamespace:        doc.LockNamespace,
			LockTimeout:          time.Duration(doc.LockTimeoutNs),
			StatementTimeout:     time.Duration(doc.StatementTimeoutNs),
			NonConcurrentIndexes: doc.NonConcurrentIndexes,
			ShadowSchema:         doc.ShadowSchema,
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
