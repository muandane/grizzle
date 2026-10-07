package plan

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ShadowSchemaPrefix is the reserved prefix for grizzle-managed shadow
// schemas. Plans may only persist shadow schema names carrying this prefix:
// SetupShadowSchema drops the schema with CASCADE before compiling, so a
// tampered plan pointing ShadowSchema at a user schema (e.g. "public")
// would destroy it.
const ShadowSchemaPrefix = "_grizzle_shadow"

// maxIdentifierLen mirrors PostgreSQL NAMEDATALEN-1: the server silently
// truncates longer identifiers, which would make setup, introspection, and
// teardown disagree on the schema name.
const maxIdentifierLen = 63

// maxTimeoutBound caps persisted timeout knobs: a tampered plan must not be
// able to disable timeouts entirely or pin absurdly large ones.
const maxTimeoutBound = 24 * time.Hour

var execIdentRegex = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// ValidateExecutionFields checks the operational (non-hash) Plan fields
// carried into Apply. It rejects values that could destroy user data or
// corrupt execution when loaded from an untrusted artifact:
//
//   - ShadowSchema, when set, must carry the _grizzle_shadow prefix, be a
//     valid SQL identifier of at most 63 bytes, and must not equal any
//     target or included schema name (SetupShadowSchema DROPs it CASCADE).
//   - LockNamespace, when set, must be a valid identifier of at most 63 bytes.
//   - LockTimeout and StatementTimeout must be non-negative and bounded.
func (p *Plan) ValidateExecutionFields() error {
	targets := map[string]bool{}
	for _, s := range p.TargetSchemas {
		targets[s] = true
	}
	if p.TargetSchema != "" {
		targets[p.TargetSchema] = true
	}
	for _, t := range p.IncludeTables {
		targets[t] = true
	}

	if p.ShadowSchema != "" {
		if !strings.HasPrefix(p.ShadowSchema, ShadowSchemaPrefix) {
			return fmt.Errorf("%w: plan shadow_schema %q must carry the %q prefix",
				ErrInvalidOptions, p.ShadowSchema, ShadowSchemaPrefix)
		}
		if err := validateExecIdent(p.ShadowSchema, "shadow_schema"); err != nil {
			return err
		}
		if targets[p.ShadowSchema] {
			return fmt.Errorf("%w: plan shadow_schema %q collides with a target/include schema",
				ErrInvalidOptions, p.ShadowSchema)
		}
	}

	if p.LockNamespace != "" {
		if err := validateExecIdent(p.LockNamespace, "lock_namespace"); err != nil {
			return err
		}
	}

	for name, d := range map[string]time.Duration{
		"lock_timeout_ns":      p.LockTimeout,
		"statement_timeout_ns": p.StatementTimeout,
	} {
		if d < 0 || d > maxTimeoutBound {
			return fmt.Errorf("%w: plan %s = %v outside [0, %v]",
				ErrInvalidOptions, name, d, maxTimeoutBound)
		}
	}
	return nil
}

func validateExecIdent(ident, field string) error {
	if len(ident) > maxIdentifierLen {
		return fmt.Errorf("%w: plan %s %q exceeds %d bytes (would be truncated by PostgreSQL)",
			ErrInvalidOptions, field, ident, maxIdentifierLen)
	}
	if !execIdentRegex.MatchString(ident) {
		return fmt.Errorf("%w: plan %s %q is not a valid SQL identifier",
			ErrInvalidOptions, field, ident)
	}
	return nil
}
