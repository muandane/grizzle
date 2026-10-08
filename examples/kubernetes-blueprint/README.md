# Kubernetes zero-downtime deployment blueprint

This guide explains how Grizzle coordinates database automigrations in multi-replica Kubernetes deployments without requiring migration init containers or pre-deployment jobs.

## The multi-replica startup race problem

In standard Kubernetes deployments without distributed locking:

1. Multiple application pods boot simultaneously during a rolling update.
2. Each pod inspects the database at the same time and attempts to execute identical `ALTER TABLE` statements.
3. PostgreSQL detects lock contention and deadlocks (`ERROR: deadlock detected`), causing pods to crash and enter `CrashLoopBackOff`.

To avoid this, teams typically resort to separate Kubernetes migration `Jobs` or `initContainers`. That introduces extra container image downloads, custom RBAC permissions, and orchestration latency.

## How Grizzle solves this in-process

Grizzle runs inside the application binary on startup (for example via `grizzle.Sync`):

```
Pod 1 (Surge)    Acquires session advisory lock ────► Runs DDL ────► Commits & Releases Lock ────► Passes /ready
Pod 2 (Surge)    Waits for the same lock ───────────────────────────► Re-diffs (0 changes)    ────► Passes /ready
Old Pods (v-1)   Serve live traffic uninterrupted throughout the entire window
```

1. **Session advisory lock:** Before inspecting or modifying schemas, the first booting pod acquires a PostgreSQL session advisory lock (`pg_try_advisory_lock` with bounded retries).
2. **Follower pod waiting:** Other replicas attempt the same lock and wait within `Options.LockTimeout`.
3. **Post-lock re-diffing:** When the leader commits and releases the lock, waiting pods acquire it, re-inspect the live catalog, find zero pending diffs, and start serving.
4. **Startup probe gating:** The `startupProbe` in `deployment.yaml` polls `/healthz` while the pod finishes migration and server init. The pod becomes `Ready` only after that work completes.
5. **Rolling update safety:** With `maxSurge: 1` and `maxUnavailable: 0`, Kubernetes keeps old replicas serving traffic until new replicas pass readiness.

Wire timeouts and scope in your application when calling Grizzle — the library does not read `GRIZZLE_*` environment variables. A typical pattern:

```go
err := grizzle.Sync(ctx, db, grizzle.Options{
	SchemaSQL:        schemaSQL,
	AllowDrop:        false,
	StrictScope:      true,
	IncludeTables:    []string{"users", "orders"},
	LockTimeout:      10 * time.Second,
	StatementTimeout: 2 * time.Minute,
})
```

Use a direct Postgres DSN or PgBouncer **session** pooling. Transaction pooling is not supported for migrations.

## Recommended Kubernetes settings

| Parameter | Recommended value | Reason |
| :--- | :--- | :--- |
| `strategy.rollingUpdate.maxSurge` | `1` (or `25%`) | Boots new replicas before terminating old replicas. |
| `strategy.rollingUpdate.maxUnavailable` | `0` | Prevents dropping below desired capacity during updates. |
| `startupProbe.failureThreshold` | `30` | Allows up to ~60s (with `periodSeconds: 2`) for large index builds. |
| `startupProbe.periodSeconds` | `2` | Checks health every 2 seconds during initialization. |

See [deployment.yaml](deployment.yaml) for a sample Deployment that sets `DATABASE_URL` from a Secret and configures probes. Map any app-specific config (timeouts, strict scope, table allow-list) in your own code or config loader before calling `grizzle.Sync`.
