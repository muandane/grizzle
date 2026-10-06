# Contributing to Grizzle

Thank you for your interest in contributing to Grizzle! We welcome pull requests, bug reports, and feedback.

## Architectural Principles & Layering Rule

Grizzle follows a strict functional core / imperative shell architecture enforced by static analysis (`architecture_test.go` and `depguard`):

```
schema  <──  scope, diff, plan, export  <──  dialect  <──  exec, history  <──  grizzle (root)
```

1. **Pure Core (`internal/schema`, `internal/scope`, `internal/diff`, `internal/plan`, `internal/export`)**:
   - Must be 100% deterministic and free of I/O.
   - **Never** import `database/sql`, `context`, `net`, `os`, `slog`, or any execution packages.
   - Sort all maps and slices before output to ensure deterministic hashes and stable golden files.
2. **Dialect Layer (`internal/dialect/postgres`, `internal/dialect/sqlite`)**:
   - The only packages that know SQL syntax, database catalogs, and DDL formatting.
   - Implements catalog introspection and DDL rendering.
3. **Execution Layer (`internal/exec`, `internal/history`)**:
   - The only package that executes queries, manages transactions, acquires advisory locks, and handles retries.
4. **Root Facade (`grizzle`)**:
   - Public API facade (`Sync`, `PlanDiff`, `Apply`, `Check`, `Export`).
   - Re-exports types via aliases (`type Plan = plan.Plan`). Minimal wiring logic only.
5. **CLI Shell (`cmd/grizzle`)**:
   - A thin CLI wrapper around the public Go library. No custom business logic in the CLI.

---

## Local Development Workflow

### Prerequisites

- Go 1.22+
- Docker or a local PostgreSQL instance (PostgreSQL 14, 15, 16, or 17)
- `golangci-lint` (v1.57+)

### Commands

Grizzle provides a `Makefile` with common targets:

```bash
# Run linter
make lint

# Run pure unit tests with race detector
make test

# Run integration tests against PostgreSQL (requires DATABASE_URL or POSTGRES_DSN)
DATABASE_URL="postgres://postgres:secret@localhost:5432/postgres?sslmode=disable" make test-integration

# Run entire local CI suite (lint + unit tests + integration tests)
DATABASE_URL="postgres://postgres:secret@localhost:5432/postgres?sslmode=disable" make ci

# Update golden plan and export fixtures after intentional changes
make golden-update
```

---

## Testing Guidelines

- **TDD First**: For bug fixes and new features, author a failing unit or integration test first, then implement the fix.
- **Race Detection**: All tests must pass with `-race` enabled (`go test -race`).
- **No Flakes**: Integration tests must be deterministic and cleanup schemas using unique test schemas or transaction rollbacks.
- **Strict Integration Tag**: Integration tests are marked with `//go:build integration` and fail loudly (never silent skip) if the database is unreachable.

---

## Commit Guidelines

All commits must follow the [Conventional Commits](https://www.conventionalcommits.org/) specification:

- `feat:` New features or capabilities (e.g. `feat(schema): support generated columns`)
- `fix:` Bug fixes (e.g. `fix(retry): halt retry after partial step progress`)
- `refactor:` Code changes that neither fix a bug nor add a feature (e.g. `refactor(exec): isolate lock connection`)
- `test:` Adding or improving tests (e.g. `test(dialect): add sqlite rebuild tests`)
- `docs:` Documentation changes (e.g. `docs(safety): update competitor citations`)
- `chore:` Maintenance tasks, dependencies, build files (e.g. `chore(ci): update github actions`)

---

## Pull Request Checklist

Before submitting a pull request, please ensure:

1. `make lint` reports 0 issues.
2. `make ci` passes completely green with `-race`.
3. Architecture import rules are strictly respected.
4. No secrets or connection strings are logged or exposed.
5. Any public API changes are documented in `docs/SPEC.md` and `CHANGELOG.md`.
