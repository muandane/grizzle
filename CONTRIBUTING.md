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

## Local Development Workflow (devenv)

Grizzle uses [devenv](https://devenv.sh) (Nix-based) as the single source of truth for the local development environment: pinned Go, golangci-lint, PostgreSQL 16, git hooks, and quality-gate scripts. **There is no Makefile** — do not add one.

### Prerequisites

- [devenv](https://devenv.sh/getting-started/) (`devenv 2.x`) and [direnv](https://direnv.net/)
- First run: `direnv allow` (or enter `devenv shell`) — this provisions Go/golangci-lint/PostgreSQL and installs the local git hooks
- `devenv up` — start the background PostgreSQL service (needed for integration tests)

### Commands

All commands are available directly in the devenv shell (mirroring the GitHub Actions CI):

```bash
# Format both Go modules
fmt

# Static analysis (go vet) on both modules
vet

# golangci-lint on both modules
lint

# Pure unit tests with race detector (both modules)
test

# Integration tests against PostgreSQL (requires `devenv up`)
DATABASE_URL="postgres://postgres:secret@localhost:5432/postgres?sslmode=disable" test-integration

# Entire local CI suite (lint + vet + unit tests + integration tests)
ci

# Update golden plan and export fixtures after intentional changes
golden-update

# Utilities
db-shell         # psql into the local test database
db-reset         # wipe and recreate the public schema
clean            # remove generated SQLite files and test artifacts
```

### Local Git Hooks (pre-push CI parity)

Entering the devenv shell installs git hooks so contributions are validated **locally before they ever touch the remote** (keeping GitHub Actions usage low):

| Hook stage | Gates |
|------------|-------|
| `pre-commit` | `gofmt` check, `go vet` (both modules), `golangci-lint` (both modules) |
| `pre-push` | `go test -race -count=1 ./...` (both modules — mirrors the CI `unit` job) |

Integration tests are intentionally **not** hooked (they require `devenv up`); run `ci` for full local parity before submitting a PR that touches `internal/exec`, `internal/history`, or the public API.

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

1. `lint` reports 0 issues (or accept the pre-commit hook gate).
2. `ci` passes completely green with `-race`.
3. Architecture import rules are strictly respected.
4. No secrets or connection strings are logged or exposed.
5. Any public API changes are documented in `docs/SPEC.md` and `CHANGELOG.md`.
