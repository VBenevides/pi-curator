# Changelog

All notable changes to the pi-curator Go executable (`curator/`) are documented here.

## [0.1.0] - 2026-10-05

First public release.

### Features

- Go CLI: `ingest`, `maintain`, `search`, `memory-search`, `read`, `index`, `state`, lifecycle, `gaps` and `recover` commands.
- Redaction, fsynced journal, episodes, optional FTS/hybrid indexing.

### Security

- History is untrusted evidence, never system policy; capture and retrieval are bound to the consented repository root.
- Secrets and denied paths are redacted before storage; transport overflow is rejected, not truncated; journal opens refuse symlinks.
