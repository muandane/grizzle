package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"time"

	"github.com/yourorg/grizzle/internal/dialect"
	"github.com/yourorg/grizzle/internal/plan"
)

// Record tracks an applied migration plan.
type Record struct {
	ID         int64     `json:"id"`
	PlanHash   string    `json:"plan_hash"`
	Status     string    `json:"status"` // "applied", "partial", "failed"
	FailedStep int       `json:"failed_step,omitzero"`
	Error      string    `json:"error,omitzero"`
	AppliedAt  time.Time `json:"applied_at"`
	DurationMs int64     `json:"duration_ms"`
	AppliedBy  string    `json:"applied_by"`
	StepsJSON  string    `json:"steps_json"`
}

// CurrentUser returns an identifier for the entity executing the migration.
func CurrentUser() string {
	u, err := user.Current()
	if err == nil && u.Username != "" {
		return u.Username
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "grizzle"
}

// EnsureTable creates the grizzle_history table if it does not already exist.
func EnsureTable(ctx context.Context, dbtx dialect.DBTX, dialectName, schemaName string) error {
	var ddl string
	switch dialectName {
	case "postgres":
		targetSchema := "public"
		if schemaName != "" {
			targetSchema = schemaName
		}
		ddl = fmt.Sprintf(`
			CREATE TABLE IF NOT EXISTS %q.grizzle_history (
				id BIGSERIAL PRIMARY KEY,
				plan_hash VARCHAR(64) NOT NULL,
				status VARCHAR(16) NOT NULL DEFAULT 'applied',
				failed_step INTEGER,
				error TEXT,
				applied_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
				duration_ms BIGINT NOT NULL,
				applied_by TEXT NOT NULL,
				steps JSONB NOT NULL
			);
			ALTER TABLE %q.grizzle_history ADD COLUMN IF NOT EXISTS status VARCHAR(16) NOT NULL DEFAULT 'applied';
			ALTER TABLE %q.grizzle_history ADD COLUMN IF NOT EXISTS failed_step INTEGER;
			ALTER TABLE %q.grizzle_history ADD COLUMN IF NOT EXISTS error TEXT;
		`, targetSchema, targetSchema, targetSchema, targetSchema)
	case "sqlite":
		ddl = `
			CREATE TABLE IF NOT EXISTS grizzle_history (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				plan_hash TEXT NOT NULL,
				status TEXT NOT NULL DEFAULT 'applied',
				failed_step INTEGER,
				error TEXT,
				applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
				duration_ms INTEGER NOT NULL,
				applied_by TEXT NOT NULL,
				steps TEXT NOT NULL
			);
		`
	default:
		return fmt.Errorf("history: unsupported dialect %q", dialectName)
	}

	_, err := dbtx.ExecContext(ctx, ddl)
	if err != nil {
		return fmt.Errorf("failed creating grizzle_history table: %w", err)
	}

	if dialectName == "sqlite" {
		// Ensure columns exist on SQLite for older tables
		_, _ = dbtx.ExecContext(ctx, "ALTER TABLE grizzle_history ADD COLUMN status TEXT NOT NULL DEFAULT 'applied';")
		_, _ = dbtx.ExecContext(ctx, "ALTER TABLE grizzle_history ADD COLUMN failed_step INTEGER;")
		_, _ = dbtx.ExecContext(ctx, "ALTER TABLE grizzle_history ADD COLUMN error TEXT;")
	}

	return nil
}

// Insert writes a migration record into the grizzle_history table.
func Insert(ctx context.Context, dbtx dialect.DBTX, dialectName, schemaName string, rec Record) error {
	var query string
	switch dialectName {
	case "postgres":
		targetSchema := "public"
		if schemaName != "" {
			targetSchema = schemaName
		}
		query = fmt.Sprintf(`
			INSERT INTO %q.grizzle_history (plan_hash, status, failed_step, error, applied_at, duration_ms, applied_by, steps)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8);
		`, targetSchema)
	case "sqlite":
		query = `
			INSERT INTO grizzle_history (plan_hash, status, failed_step, error, applied_at, duration_ms, applied_by, steps)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?);
		`
	default:
		return fmt.Errorf("history: unsupported dialect %q", dialectName)
	}

	if rec.Status == "" {
		rec.Status = "applied"
	}
	if rec.AppliedAt.IsZero() {
		rec.AppliedAt = time.Now().UTC()
	}
	if rec.AppliedBy == "" {
		rec.AppliedBy = CurrentUser()
	}

	var failedStep sql.NullInt64
	if rec.FailedStep > 0 {
		failedStep = sql.NullInt64{Int64: int64(rec.FailedStep), Valid: true}
	}
	var errText sql.NullString
	if rec.Error != "" {
		errText = sql.NullString{String: rec.Error, Valid: true}
	}

	_, err := dbtx.ExecContext(ctx, query, rec.PlanHash, rec.Status, failedStep, errText, rec.AppliedAt, rec.DurationMs, rec.AppliedBy, rec.StepsJSON)
	if err != nil {
		return fmt.Errorf("failed writing grizzle_history record: %w", err)
	}
	return nil
}

// RecordPlan serializes the plan steps and inserts an 'applied' record into grizzle_history.
func RecordPlan(ctx context.Context, dbtx dialect.DBTX, dialectName, schemaName string, p *plan.Plan, duration time.Duration) error {
	return RecordProgress(ctx, dbtx, dialectName, schemaName, p, "applied", 0, nil, duration)
}

// RecordProgress serializes the plan steps and inserts a record with explicit status into grizzle_history.
func RecordProgress(ctx context.Context, dbtx dialect.DBTX, dialectName, schemaName string, p *plan.Plan, status string, failedStep int, execErr error, duration time.Duration) error {
	if err := EnsureTable(ctx, dbtx, dialectName, schemaName); err != nil {
		return err
	}

	stepsJSON, err := json.Marshal(p.Steps)
	if err != nil {
		return fmt.Errorf("failed serializing plan steps: %w", err)
	}

	var errMsg string
	if execErr != nil {
		errMsg = execErr.Error()
	}

	return Insert(ctx, dbtx, dialectName, schemaName, Record{
		PlanHash:   p.Hash(),
		Status:     status,
		FailedStep: failedStep,
		Error:      errMsg,
		AppliedAt:  time.Now().UTC(),
		DurationMs: duration.Milliseconds(),
		AppliedBy:  CurrentUser(),
		StepsJSON:  string(stepsJSON),
	})
}

// GetLatest returns the most recently applied migration record, or nil if no history exists.
func GetLatest(ctx context.Context, dbtx dialect.DBTX, dialectName, schemaName string) (*Record, error) {
	var query string
	switch dialectName {
	case "postgres":
		targetSchema := "public"
		if schemaName != "" {
			targetSchema = schemaName
		}
		query = fmt.Sprintf(`
			SELECT id, plan_hash, status, failed_step, error, applied_at, duration_ms, applied_by, steps::text
			FROM %q.grizzle_history
			ORDER BY id DESC
			LIMIT 1;
		`, targetSchema)
	case "sqlite":
		query = `
			SELECT id, plan_hash, status, failed_step, error, applied_at, duration_ms, applied_by, steps
			FROM grizzle_history
			ORDER BY id DESC
			LIMIT 1;
		`
	default:
		return nil, fmt.Errorf("history: unsupported dialect %q", dialectName)
	}

	var rec Record
	var failedStep sql.NullInt64
	var errText sql.NullString
	err := dbtx.QueryRowContext(ctx, query).Scan(&rec.ID, &rec.PlanHash, &rec.Status, &failedStep, &errText, &rec.AppliedAt, &rec.DurationMs, &rec.AppliedBy, &rec.StepsJSON)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("querying latest grizzle_history: %w", err)
	}
	if failedStep.Valid {
		rec.FailedStep = int(failedStep.Int64)
	}
	if errText.Valid {
		rec.Error = errText.String
	}
	return &rec, nil
}

// List returns all migration records ordered by ID ascending.
func List(ctx context.Context, dbtx dialect.DBTX, dialectName, schemaName string) ([]Record, error) {
	var query string
	switch dialectName {
	case "postgres":
		targetSchema := "public"
		if schemaName != "" {
			targetSchema = schemaName
		}
		query = fmt.Sprintf(`
			SELECT id, plan_hash, status, failed_step, error, applied_at, duration_ms, applied_by, steps::text
			FROM %q.grizzle_history
			ORDER BY id ASC;
		`, targetSchema)
	case "sqlite":
		query = `
			SELECT id, plan_hash, status, failed_step, error, applied_at, duration_ms, applied_by, steps
			FROM grizzle_history
			ORDER BY id ASC;
		`
	default:
		return nil, fmt.Errorf("history: unsupported dialect %q", dialectName)
	}

	rows, err := dbtx.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("querying grizzle_history: %w", err)
	}
	defer rows.Close()

	var records []Record
	for rows.Next() {
		var rec Record
		var failedStep sql.NullInt64
		var errText sql.NullString
		if err := rows.Scan(&rec.ID, &rec.PlanHash, &rec.Status, &failedStep, &errText, &rec.AppliedAt, &rec.DurationMs, &rec.AppliedBy, &rec.StepsJSON); err != nil {
			return nil, fmt.Errorf("scanning grizzle_history: %w", err)
		}
		if failedStep.Valid {
			rec.FailedStep = int(failedStep.Int64)
		}
		if errText.Valid {
			rec.Error = errText.String
		}
		records = append(records, rec)
	}
	return records, rows.Err()
}
