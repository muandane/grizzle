# Security Policy

The Grizzle team takes security vulnerabilities seriously. We appreciate responsible disclosure to help protect users of the library.

## Supported Versions

Only the latest minor release receives active security patches.

| Version | Supported          |
| :------ | :----------------- |
| `v0.1.x`| :white_check_mark: |
| `< 0.1` | :x:                |

## Reporting a Vulnerability

If you discover a security vulnerability in Grizzle, please report it privately. **Do not create public GitHub issues or discussions for security concerns.**

### Disclosure Process

1. Email your report to **zine@omnivya.fr** (or open a private security advisory via GitHub Security Advisory if available).
2. Include in your report:
   - A clear description of the vulnerability.
   - Proof-of-concept (PoC) code or reproduction steps.
   - Impact assessment (e.g., SQL injection, lock bypass, unintended data loss, credential disclosure).
   - Affected Grizzle versions and database engines (PostgreSQL / SQLite).

### Response SLA

- **Initial Response**: We will acknowledge receipt of your report within **48 hours**.
- **Assessment & Triage**: We will provide a triage confirmation and severity assessment within **5 business days**.
- **Patch & Advisory**: Once resolved, we will publish a patch release alongside a CVE advisory with appropriate credit to the reporter.

## In-Scope Vulnerabilities

- **SQL Injection**: Injection risks in DDL generation, identifier quoting, or catalog introspection queries.
- **Unintended Data Loss / Drop Bypass**: Circumvention of `AllowDrop: false` or hazard gating (`ErrHazardBlocked`) leading to silent schema drops or data destruction.
- **Distributed Lock Bypass**: Concurrency flaws that allow concurrent processes to execute conflicting DDL simultaneously despite advisory locking.
- **Credential Disclosure**: Failure to redact database credentials in structured logs, CLI outputs, or serialized plan artifacts.

## Out of Scope

- Attacks requiring direct, authorized superuser access to the database engine.
- Vulnerabilities in underlying third-party drivers (`pgx`, `modernc.org/sqlite`) that have not been demonstrated to be exploitable via Grizzle.
- Social engineering, denial of service on local test runners, or physical attacks.
