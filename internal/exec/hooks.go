package exec

import (
	"context"
	"fmt"
	"runtime/debug"

	"github.com/muandane/grizzle/internal/dialect"
	"github.com/muandane/grizzle/internal/plan"
)

// HookContext provides invocation context and database access for a step hook.
// DBTX is bound to the executor of the pending step: a *sql.Tx for
// transactional groups or a *sql.Conn for non-transactional steps.
type HookContext struct {
	// Context is the migration execution context.
	Context context.Context
	// DBTX is the database executor bound to the current step or sync phase.
	DBTX dialect.DBTX
	// Step is the pending (BeforeStep) or just-executed (AfterStep) plan step.
	Step plan.Step
	// Index is the 1-based index of the current step.
	Index int
	// Total is the total number of steps in the plan.
	Total int
	// IsNonTx reports whether the step runs outside a transaction.
	IsNonTx bool
}

// StepHook executes custom imperative code immediately before or after each
// plan step. Hooks must be idempotent: if a later step fails and the migration
// is retried or resumed after partial execution, hooks may be invoked again.
type StepHook func(ctx HookContext) error

// SyncHook executes custom imperative code once before or after the entire
// synchronization. It receives a dedicated connection (not a transaction)
// because the execution may contain non-transactional statements.
// Hooks must be idempotent for the same reason as StepHook.
type SyncHook func(ctx context.Context, dbtx dialect.DBTX) error

// recoverHookPanic converts a panic escaping a user hook into an error that
// carries the goroutine stack, so a faulty hook cannot crash the host process.
func recoverHookPanic(panicResult any) error {
	return fmt.Errorf("grizzle: hook panicked: %v\n%s", panicResult, debug.Stack())
}

// callBeforeStep invokes the BeforeStep hook if set. Hook panics are recovered
// and returned as errors.
func callBeforeStep(hook StepHook, hc HookContext) (err error) {
	if hook == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			err = recoverHookPanic(r)
		}
	}()
	return hook(hc)
}

// callAfterStep invokes the AfterStep hook if set. Hook panics are recovered
// and returned as errors.
func callAfterStep(hook StepHook, hc HookContext) (err error) {
	if hook == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			err = recoverHookPanic(r)
		}
	}()
	return hook(hc)
}

// callBeforeSync invokes the BeforeSync hook if set. Hook panics are recovered
// and returned as errors.
func callBeforeSync(hook SyncHook, ctx context.Context, dbtx dialect.DBTX) (err error) {
	if hook == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			err = recoverHookPanic(r)
		}
	}()
	return hook(ctx, dbtx)
}

// callAfterSync invokes the AfterSync hook if set. Hook panics are recovered
// and returned as errors.
func callAfterSync(hook SyncHook, ctx context.Context, dbtx dialect.DBTX) (err error) {
	if hook == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			err = recoverHookPanic(r)
		}
	}()
	return hook(ctx, dbtx)
}
