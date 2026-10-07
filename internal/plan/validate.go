package plan

import (
	"fmt"
	"time"
)

// maxTimeoutBound caps persisted timeout knobs: a tampered plan must not be
// able to disable timeouts entirely or pin absurdly large ones.
const maxTimeoutBound = 24 * time.Hour

// ValidateExecutionFields checks the operational (non-hash) Plan fields
// carried into Apply. Lock identity and shadow schema names are never
// persisted in the artifact; only timeout bounds are validated here.
func (p *Plan) ValidateExecutionFields() error {
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
