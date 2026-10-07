# Contributing to Grizzle

Pull requests, bug reports, and feedback are welcome. Start with the short setup below, then the layering rules and PR checklist.

For a README-facing overview of the same workflow, see [Contributing](README.md#contributing).

---

## Local setup (devenv)

Grizzle uses [devenv](https://devenv.sh) (Nix) as the single source of truth for pinned Go, golangci-lint, PostgreSQL 16, git hooks, and quality-gate scripts. There is no Makefile; do not add one.

### Prerequisites

| Step | Action |
| :--- | :--- |
| 1 | Install [devenv](https://devenv.sh/getting-started/) (`2.x`) and [direnv](https://direnv.net/) |
| 2 | From the repo root: `direnv allow` (or `devenv shell`) — provisions tools and installs git hooks |
| 3 | `devenv up` — starts background PostgreSQL (required for integration tests) |

Default connection strings (set by devenv):

```text
DATABASE_URL=postgres://127.0.0.1:5432/grizzle_test?sslmode=disable
POSTGRES_DSN=postgres://127.0.0.1:5432/grizzle_test?sslmode=disable
```

### Commands

Available in the devenv shell (same gates as GitHub Actions):

```bash
fmt                 # Format both Go modules
vet                 # go vet on both modules
lint                # golangci-lint on both modules
test                # Unit tests with race detector (both modules)
test-integration    # Integration tests (requires devenv up)
ci                  # lint + vet + unit + integration
golden-update       # Regenerate golden plan/export fixtures
db-shell            # psql into the local test database
db-reset            # Wipe and recreate the public schema
clean               # Remove generated SQLite files and test artifacts
```

### Git hooks

Entering the devenv shell installs hooks so checks run locally before they hit CI:

| Hook | Gates |
| :--- | :--- |
| `pre-commit` | `gofmt`, `go vet`, `golangci-lint` (both modules) |
| `pre-push` | `go test -race -count=1 ./...` (both modules — mirrors the CI `unit` job) |

Integration tests are not hooked (they need `devenv up`). Run `ci` before a PR that touches `internal/exec`, `internal/history`, or the public API.

---

## Architectural layering

Enforced by `architecture_test.go` and `depguard`:

```
schema  <──  scope, diff, plan, export, lint  <──  dialect  <──  exec, history  <──  grizzle (root)
```

1. **Pure core** (`internal/schema`, `scope`, `diff`, `plan`, `export`, `lint`)
   - Deterministic; no I/O.
   - Never import `database/sql`, `context`, `net`, `os`, `slog`, or execution packages.
   - Sort maps and slices before output so hashes and golden files stay stable.
2. **Dialect** (`internal/dialect/postgres`, `internal/dialect/sqlite`)
   - Only packages that know SQL syntax, catalogs, and DDL formatting.
3. **Execution** (`internal/exec`, `internal/history`)
   - Only packages that run queries, manage transactions, acquire advisory locks, and retry.
4. **Root facade** (`grizzle`)
   - Public API (`Sync`, `PlanDiff`, `Apply`, `Check`, `Export`) and type aliases. Minimal wiring.
5. **CLI** (`cmd/grizzle`)
   - Thin wrapper around the public library. No business logic in the CLI.

---

## Testing

- Write a failing unit or integration test first for bug fixes and new features.
- All tests must pass with `-race` (`go test -race`).
- Integration tests must be deterministic and clean up via unique schemas or rollbacks.
- Integration tests use `//go:build integration` and fail loudly if the database is unreachable (never silent skip).

---

## Commits

Follow [Conventional Commits](https://www.conventionalcommits.org/):

| Prefix | Use for |
| :--- | :--- |
| `feat:` | New capability |
| `fix:` | Bug fix |
| `refactor:` | Change with no feature or fix |
| `test:` | Tests only |
| `docs:` | Documentation |
| `chore:` | Maintenance, deps, build |

Examples: `feat(schema): support generated columns`, `fix(retry): halt retry after partial step progress`.

---

## Pull request checklist

1. `lint` reports zero issues (or the pre-commit hook passes).
2. `ci` is green with `-race`.
3. Layering / import rules are respected.
4. No secrets or connection strings are logged or committed.
5. Public API changes are reflected in `docs/SPEC.md` and `CHANGELOG.md`.
