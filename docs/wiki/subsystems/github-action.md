# Subsystem: GitHub Action

## Overview
The Grizzle GitHub Action (`action.yml`) provides automated CI/CD integration for linting, diff calculation, plan simulation, and migration execution.

## Sticky Pull Request Summary Comment
When invoked on `pull_request` events with `comment: 'true'`, the action posts or updates an in-place sticky comment summarizing the migration plan.

### Mechanics
1. **Idempotent Anchor**: Comments contain an invisible HTML tag `<!-- grizzle-action-comment -->`.
2. **In-Place Updates**: On subsequent commits, the action queries the GitHub Issues/PR Comments API for existing comments with this anchor. If found, it updates the existing comment via `PATCH` rather than posting new messages.
3. **Structured Metrics**: Displays badges for `+ Adds`, `~ Alters`, and `- Drops`, collapsible SQL DDL diff blocks, hazard warnings, and simulation metrics.
