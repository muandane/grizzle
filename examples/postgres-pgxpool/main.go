package main

import (
	"context"
	_ "embed"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/muandane/grizzle"
)

//go:embed schema.sql
var schemaSQL string

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:password@localhost:5432/myapp?sslmode=disable"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	targetSchema := os.Getenv("PG_SCHEMA")

	// 1. Configure and initialize native pgx connection pool
	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		log.Fatalf("parsing connection config failed: %v", err)
	}
	if targetSchema != "" {
		if poolConfig.ConnConfig.RuntimeParams == nil {
			poolConfig.ConnConfig.RuntimeParams = make(map[string]string)
		}
		poolConfig.ConnConfig.RuntimeParams["search_path"] = targetSchema
	}
	poolConfig.MaxConns = 15
	poolConfig.MinConns = 2
	poolConfig.MaxConnIdleTime = 5 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		log.Fatalf("initializing pgxpool failed: %v", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("pinging postgres failed: %v", err)
	}

	// 2. Wrap pool with stdlib.OpenDBFromPool for in-process automigration
	// This shares the existing connection pool without allocating new connection handles.
	sqlDB := stdlib.OpenDBFromPool(pool)

	log.Println("Synchronizing database schema using pgx connection pool...")
	err = grizzle.Sync(ctx, sqlDB, grizzle.Options{
		TargetSchema:     targetSchema,
		SchemaSQL:        schemaSQL,
		AllowDrop:        false,
		LockTimeout:      5 * time.Second,
		StatementTimeout: 2 * time.Minute,
	})
	if err != nil {
		log.Fatalf("grizzle schema sync failed: %v", err)
	}

	// 3. Application operations proceed using native high-performance pgxpool methods
	var insertedID int64
	service := fmt.Sprintf("api-server-%d", time.Now().UnixNano())
	err = pool.QueryRow(ctx,
		"INSERT INTO metrics (service_name, cpu_usage, memory_mb) VALUES ($1, $2, $3) RETURNING id",
		service, 14.5, 512,
	).Scan(&insertedID)
	if err != nil {
		log.Fatalf("native pgxpool insert failed: %v", err)
	}

	var rowCount int
	err = pool.QueryRow(ctx, "SELECT COUNT(*) FROM metrics WHERE service_name = $1", service).Scan(&rowCount)
	if err != nil {
		log.Fatalf("native pgxpool count failed: %v", err)
	}

	log.Printf("Recorded metric row %d for %s (total matching: %d)", insertedID, service, rowCount)
}
