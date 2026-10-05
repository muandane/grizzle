package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/yourorg/grizzle"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: grizzle <plan|apply|check> [flags]")
		os.Exit(1)
	}

	command := os.Args[1]
	fs := flag.NewFlagSet(command, flag.ExitOnError)
	dsn := fs.String("dsn", os.Getenv("DATABASE_URL"), "Database DSN connection string")
	schemaFile := fs.String("schema", "schema.sql", "Path to schema SQL file")
	allowDrop := fs.Bool("allow-drop", false, "Permit destructive operations")
	outputJSON := fs.Bool("json", false, "Output plan in JSON format")
	outputFile := fs.String("out", "", "File path to write plan output to")
	planFile := fs.String("plan", "", "Path to plan JSON file for apply")
	expectedHash := fs.String("expected-hash", "", "Expected plan approval hash")
	_ = fs.Parse(os.Args[2:])

	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "Error: database DSN is required (via -dsn or DATABASE_URL)")
		os.Exit(1)
	}

	db, err := sql.Open("pgx", *dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	ctx := context.Background()

	switch command {
	case "plan":
		content, err := os.ReadFile(*schemaFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading schema file %q: %v\n", *schemaFile, err)
			os.Exit(1)
		}
		opts := grizzle.Options{
			SchemaSQL: string(content),
			AllowDrop: *allowDrop,
		}
		p, err := grizzle.PlanDiff(ctx, db, opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Plan error: %v\n", err)
			os.Exit(1)
		}

		if *outputJSON {
			data, err := json.MarshalIndent(p, "", "  ")
			if err != nil {
				fmt.Fprintf(os.Stderr, "JSON marshal error: %v\n", err)
				os.Exit(1)
			}
			if *outputFile != "" {
				if err := os.WriteFile(*outputFile, data, 0644); err != nil {
					fmt.Fprintf(os.Stderr, "Writing plan error: %v\n", err)
					os.Exit(1)
				}
			} else {
				fmt.Println(string(data))
			}
		} else {
			_ = p.Format(os.Stdout, true)
		}
		fmt.Fprintf(os.Stderr, "Plan Hash: %s\n", p.Hash())

	case "apply":
		if *planFile != "" {
			planData, err := os.ReadFile(*planFile)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error reading plan file %q: %v\n", *planFile, err)
				os.Exit(1)
			}
			var p grizzle.Plan
			if err := json.Unmarshal(planData, &p); err != nil {
				fmt.Fprintf(os.Stderr, "Error parsing plan JSON: %v\n", err)
				os.Exit(1)
			}
			applyOpts := grizzle.ApplyOpts{
				ExpectedHash: *expectedHash,
			}
			if err := grizzle.Apply(ctx, db, &p, applyOpts); err != nil {
				fmt.Fprintf(os.Stderr, "Apply error: %v\n", err)
				os.Exit(1)
			}
		} else {
			content, err := os.ReadFile(*schemaFile)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error reading schema file %q: %v\n", *schemaFile, err)
				os.Exit(1)
			}
			opts := grizzle.Options{
				SchemaSQL: string(content),
				AllowDrop: *allowDrop,
			}
			if *expectedHash != "" {
				p, err := grizzle.PlanDiff(ctx, db, opts)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Plan error: %v\n", err)
					os.Exit(1)
				}
				if err := grizzle.Apply(ctx, db, p, grizzle.ApplyOpts{ExpectedHash: *expectedHash}); err != nil {
					fmt.Fprintf(os.Stderr, "Apply error: %v\n", err)
					os.Exit(1)
				}
			} else {
				if err := grizzle.Sync(ctx, db, opts); err != nil {
					fmt.Fprintf(os.Stderr, "Apply error: %v\n", err)
					os.Exit(1)
				}
			}
		}
		fmt.Println("Schema applied successfully.")

	case "check":
		content, err := os.ReadFile(*schemaFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading schema file %q: %v\n", *schemaFile, err)
			os.Exit(1)
		}
		opts := grizzle.Options{
			SchemaSQL: string(content),
			AllowDrop: *allowDrop,
		}
		p, err := grizzle.PlanDiff(ctx, db, opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Check error: %v\n", err)
			os.Exit(1)
		}
		if len(p.Steps) > 0 {
			fmt.Fprintf(os.Stderr, "Drift detected: %d pending changes (hash: %s)\n", len(p.Steps), p.Hash())
			os.Exit(2)
		}
		fmt.Println("Database is in sync.")

	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %q. Expected plan, apply, or check.\n", command)
		os.Exit(1)
	}
}
