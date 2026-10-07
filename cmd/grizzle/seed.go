package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/muandane/grizzle"
)

// runSeed executes the idempotent seed SQL from --seed against --dsn.
// Exit codes: 0 = applied or skipped, 1 = failure.
func runSeed(ctx context.Context, dsn, seedFile string, force bool) int {
	if seedFile == "" {
		slog.Error("seed file is required (--seed <path>)")
		return 1
	}
	db, err := initDB(dsn)
	if err != nil {
		return 1
	}
	defer func() { _ = db.Close() }()

	content, err := os.ReadFile(filepath.Clean(seedFile)) //nolint:gosec // G304: CLI accepts user-provided seed file path
	if err != nil {
		slog.Error("reading seed file", "path", seedFile, "err", err)
		return 1
	}

	seedHash := grizzle.SeedHash(string(content))
	if err := grizzle.Seed(ctx, db, string(content), grizzle.WithSeedForce(force)); err != nil {
		slog.Error("seed execution failed", "err", err)
		return 1
	}

	if force {
		fmt.Printf("Seed applied (forced): %s\n", seedHash)
	} else {
		fmt.Printf("Seed OK (applied or already applied): %s\n", seedHash)
	}
	return 0
}
