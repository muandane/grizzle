package main

import (
	"context"
	"database/sql"
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
	_ = fs.Parse(os.Args[2:])

	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "Error: database DSN is required (via -dsn or DATABASE_URL)")
		os.Exit(1)
	}

	content, err := os.ReadFile(*schemaFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading schema file %q: %v\n", *schemaFile, err)
		os.Exit(1)
	}

	db, err := sql.Open("pgx", *dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	ctx := context.Background()
	opts := grizzle.Options{
		SchemaSQL: string(content),
		AllowDrop: *allowDrop,
	}

	switch command {
	case "plan":
		p, err := grizzle.PlanDiff(ctx, db, opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Plan error: %v\n", err)
			os.Exit(1)
		}
		_ = p.Format(os.Stdout, true)
	case "apply":
		if err := grizzle.Sync(ctx, db, opts); err != nil {
			fmt.Fprintf(os.Stderr, "Apply error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Schema applied successfully.")
	case "check":
		p, err := grizzle.PlanDiff(ctx, db, opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Check error: %v\n", err)
			os.Exit(1)
		}
		if len(p.Steps) > 0 {
			fmt.Fprintf(os.Stderr, "Drift detected: %d pending changes\n", len(p.Steps))
			os.Exit(2)
		}
		fmt.Println("Database is in sync.")
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %q. Expected plan, apply, or check.\n", command)
		os.Exit(1)
	}
}
