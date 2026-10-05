package history

import (
	"time"
)

// Record tracks an applied migration plan.
type Record struct {
	ID         int64     `json:"id"`
	PlanHash   string    `json:"plan_hash"`
	AppliedAt  time.Time `json:"applied_at"`
	DurationMs int64     `json:"duration_ms"`
	AppliedBy  string    `json:"applied_by"`
	StepsJSON  string    `json:"steps_json"`
}
