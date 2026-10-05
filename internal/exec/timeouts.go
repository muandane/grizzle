package exec

import (
	"context"
	"fmt"
	"time"

	"github.com/muandane/grizzle/internal/dialect"
)

const (
	// DefaultLockTimeout specifies the default timeout for acquiring table/advisory locks.
	DefaultLockTimeout = 5 * time.Second

	// DefaultStatementTimeout specifies the default timeout for executing any single migration DDL statement.
	DefaultStatementTimeout = 5 * time.Minute

	// DefaultMaxRetries specifies the default number of retry attempts upon lock timeout conflicts.
	DefaultMaxRetries = 3
)

// ApplyTxTimeouts sets lock_timeout and statement_timeout locally for the current transaction.
func ApplyTxTimeouts(ctx context.Context, dbtx dialect.DBTX, lockTimeout, stmtTimeout time.Duration) error {
	if lockTimeout > 0 {
		lockMs := lockTimeout.Milliseconds()
		if _, err := dbtx.ExecContext(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms';", lockMs)); err != nil {
			return fmt.Errorf("failed setting tx lock_timeout: %w", err)
		}
	}
	if stmtTimeout > 0 {
		stmtMs := stmtTimeout.Milliseconds()
		if _, err := dbtx.ExecContext(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms';", stmtMs)); err != nil {
			return fmt.Errorf("failed setting tx statement_timeout: %w", err)
		}
	}
	return nil
}

// ApplySessionTimeouts sets lock_timeout and statement_timeout on the dedicated session connection.
func ApplySessionTimeouts(ctx context.Context, dbtx dialect.DBTX, lockTimeout, stmtTimeout time.Duration) error {
	if lockTimeout > 0 {
		lockMs := lockTimeout.Milliseconds()
		if _, err := dbtx.ExecContext(ctx, fmt.Sprintf("SET lock_timeout = '%dms';", lockMs)); err != nil {
			return fmt.Errorf("failed setting session lock_timeout: %w", err)
		}
	}
	if stmtTimeout > 0 {
		stmtMs := stmtTimeout.Milliseconds()
		if _, err := dbtx.ExecContext(ctx, fmt.Sprintf("SET statement_timeout = '%dms';", stmtMs)); err != nil {
			return fmt.Errorf("failed setting session statement_timeout: %w", err)
		}
	}
	return nil
}
