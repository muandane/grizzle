# Kubernetes zero-downtime deployment blueprint

This guide explains how Grizzle coordinates database automigrations in multi-replica Kubernetes deployments without requiring migration init containers or pre-deployment jobs.

## The multi-replica startup race problem

In standard Kubernetes deployments without distributed locking:
1. Multiple application pods boot simultaneously during a rolling update.
2. Each pod inspects the database at the same time and attempts to execute identical `ALTER TABLE` statements.
3. PostgreSQL detects lock contention and deadlocks (`ERROR: deadlock detected`), causing pods to crash and enter `CrashLoopBackOff`.

To avoid this, teams typically resort to separate Kubernetes migration `Jobs` or `initContainers`. This introduces extra container image downloads, custom RBAC permissions, and orchestration latency.

## How Grizzle solves this in-process

Grizzle runs directly within the application binary on startup:

```
Pod 1 (Surge)    Acquires pg_advisory_lock ────► Runs DDL ────► Commits & Releases Lock ────► Passes /ready
Pod 2 (Surge)    Waits on pg_advisory_lock ───────────────────► Re-diffs (0 changes)    ────► Passes /ready
Old Pods (v-1)   Serve live traffic uninterrupted throughout the entire window
```

1. **Dedicated advisory lock:** Before inspecting or modifying schemas, the first booting pod acquires a session-level PostgreSQL advisory lock (`pg_advisory_lock`).
2. **Follower pod waiting:** Other replicas attempt to acquire the lock and block safely in PostgreSQL.
3. **Post-lock re-diffing:** When the first pod commits and releases the lock, the waiting pods acquire the lock and re-inspect the live database catalog. They find zero pending diffs and start their HTTP listeners immediately without executing redundant statements.
4. **Startup probe gating:** The `startupProbe` in `deployment.yaml` polls `/healthz` while the pod completes migration and server initialization. The pod only transitions to `Ready` once migrations are complete.
5. **Rolling update safety:** With `maxSurge: 1` and `maxUnavailable: 0`, Kubernetes keeps old replicas serving traffic until new replicas pass readiness checks.

## Recommended configuration

Apply these settings in production deployment manifests:

| Parameter | Recommended value | Reason |
| :--- | :--- | :--- |
| `strategy.rollingUpdate.maxSurge` | `1` (or `25%`) | Boots new replicas before terminating old replicas. |
| `strategy.rollingUpdate.maxUnavailable` | `0` | Prevents dropping below desired capacity during updates. |
| `startupProbe.failureThreshold` | `30` | Allows up to 60 seconds for large index builds or migrations to finish. |
| `startupProbe.periodSeconds` | `2` | Checks health every 2 seconds during initialization. |
| `GRIZZLE_LOCK_TIMEOUT` | `10s` | Maximum wait duration for lock acquisition before retrying with backoff. |
| `GRIZZLE_STATEMENT_TIMEOUT` | `2m` | Aborts any individual DDL statement exceeding 2 minutes. |
| `GRIZZLE_STRICT_SCOPE` | `true` | Restricts operations to explicitly declared application tables. |
