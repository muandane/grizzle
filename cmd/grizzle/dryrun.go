package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/muandane/grizzle"
)

// runDryRunApply verifies the planned DDL against live data without
// persisting any change (live dry-run rollback verification).
func runDryRunApply(ctx context.Context, db *sql.DB, planFile, schemaFile, rolesSQL string, allowDrop bool, acceptedCodes []grizzle.HazardCode, renames map[string]string, expandContract bool) int {
	if planFile != "" {
		planData, err := os.ReadFile(filepath.Clean(planFile)) //nolint:gosec // G304: CLI accepts user-provided plan file path
		if err != nil {
			slog.Error("reading plan file", "path", planFile, "err", err)
			return 2
		}
		p, recordedHash, err := grizzle.ParsePlanJSON(planData)
		if err != nil {
			slog.Error("parsing plan JSON", "err", err)
			return 2
		}
		res, err := grizzle.DryRunVerifyPlan(ctx, db, p, grizzle.ApplyOpts{
			ExpectedHash:  recordedHash,
			AcceptHazards: acceptedCodes,
		})
		if err != nil {
			slog.Error("dry-run verification failed", "err", err)
			fmt.Fprintln(os.Stderr, "Dry-run verification FAILED: the migration would not apply cleanly to live data.")
			return 1
		}
		printDryRunResult(res)
		return 0
	}

	content, err := os.ReadFile(filepath.Clean(schemaFile)) //nolint:gosec // G304: CLI accepts user-provided schema file path
	if err != nil {
		slog.Error("reading schema file", "path", schemaFile, "err", err)
		return 2
	}
	opts := grizzle.Options{
		SchemaSQL:      string(content),
		RolesSQL:       rolesSQL,
		AllowDrop:      allowDrop,
		AcceptHazards:  acceptedCodes,
		Renames:        renames,
		ExpandContract: expandContract,
	}
	res, err := grizzle.DryRunVerify(ctx, db, opts)
	if err != nil {
		slog.Error("dry-run verification failed", "err", err)
		fmt.Fprintln(os.Stderr, "Dry-run verification FAILED: the migration would not apply cleanly to live data.")
		return 1
	}
	printDryRunResult(res)
	return 0
}

func printDryRunResult(res *grizzle.DryRunResult) {
	fmt.Printf("Dry-run verification passed: %d step(s) executed and rolled back, %d non-transactional step(s) unverified, duration %s.\n",
		res.ExecutedSteps, len(res.UnverifiedNonTx), res.Duration)
	for _, s := range res.UnverifiedNonTx {
		fmt.Printf("  unverified (non-transactional): [%s] %s\n", s.Type, s.SQL)
	}
	fmt.Fprintln(os.Stderr, "No changes were persisted (dry-run).")
}
