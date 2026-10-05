# Changelog

All notable changes to the pi-curator Pi/OMP plugin (`plugin/`) are documented here.

## [0.1.0] - 2026-10-05

First public release.

### Features

- Pi/OMP extension: consent prompt, automatic capture, `memory_search`/`memory_read` tools, static recall guidance and task prefetch.

### Security

- History is untrusted evidence, never system policy; capture and retrieval are bound to the consented repository root.
- Secrets and denied paths are redacted before storage; transport overflow is rejected, not truncated; journal opens refuse symlinks.
