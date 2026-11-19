# Subsystem: Migration Simulation Sandbox

## Overview
Grizzle provides pre-flight execution simulation via `grizzle plan --simulate` and `grizzle.Simulate(ctx, db, opts)`. The simulation executes planned DDL inside an isolated transaction (or savepoint for SQLite) against live data to profile lock hold duration and detect slow constraint validations before touching live tables.

## Safety Invariant
The simulation guarantees zero persistent schema or data mutation:
1. All executed DDL steps run inside a dedicated connection.
2. An unconditional `ROLLBACK` (or `ROLLBACK TO SAVEPOINT`) is executed upon completion or error.
3. No migration history is recorded in `grizzle_history`.

## Profiling & Thresholds
For each migration step, the sandbox records:
- `Duration`: exact execution time of the step.
- `ExceededLockMax`: boolean flag set when the step execution time exceeds the configured `--max-lock-duration` (default 500ms).

When threshold violations occur, the runner emits structured warnings highlighting the statement and duration.
