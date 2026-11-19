# Subsystem: Declarative SQL Lint Engine

## Overview
Grizzle includes an in-memory, AST-based declarative schema linter in `internal/lint`. The linter operates on the compiled `*schema.Schema` intermediate representation produced by shadow compilation. Diagnostics are 100% deterministic for identical input DDL and require zero database connectivity.

## Rule Structure
Each rule implements the pure `Rule` interface:
```go
type Rule interface {
    ID() string
    Description() string
    Check(s *schema.Schema) []Diagnostic
}
```

## Profiles
1. **Standard (`ProfileStandard`)**: Built-in rules enabled by default (`L001..L013` and `L032`). Enforces fundamental correctness (primary keys, unindexed foreign keys, naming conventions, idempotent seeds, financial data types).
2. **Strict (`ProfileStrict`)**: Standard rules plus production hygiene rules (`ExtendedRules()`). Enforces strict primary key types, keyword guards, boolean defaults, index deduplication, table comments, cascade safety, and index column limits.
3. **All (`ProfileAll`)**: Enables all rules registered in the engine.

## Suppression Syntax
In-source comment annotations allow operators to suppress specific diagnostics:
- `-- grizzle:lint-ignore L001, L032`: Ignores the specified rules on the subsequent statement.
- `-- grizzle:lint-disable L033`: Disables the rule for the remainder of the file.
- `-- grizzle:lint-enable L033`: Re-enables the rule.
