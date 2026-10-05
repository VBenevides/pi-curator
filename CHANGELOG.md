# Changelog

All notable changes to pi-curator are documented here. Component detail: [curator/CHANGELOG.md](curator/CHANGELOG.md), [plugin/CHANGELOG.md](plugin/CHANGELOG.md).

## [0.1.0] - 2026-10-05

First public release.

### Features

- Consent-gated capture of visible messages and tool traffic into a redacted, fsynced, append-only journal.
- Native `memory_search` and `memory_read` tools with total token budgets, byte-cursor continuation and explicit capture-gap reporting.
- Episodes, lifecycle commands (pin, supersede, archive), scoped state declarations and optional SQLite/FTS and hybrid indexed retrieval.
- Task-conditioned prefetch (500 estimated tokens by default; `PI_CURATOR_PREFETCH_BUDGET=0` opts out).
- Offline retrieval evaluation script `scripts/evaluate_retrieval.py`.

### Security

- History is untrusted evidence, never system policy; capture and retrieval are bound to the consented repository root.
- Secrets and denied paths are redacted before storage; transport overflow is rejected, not truncated; journal opens refuse symlinks.
