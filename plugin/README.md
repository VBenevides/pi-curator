# pi-curator plugin

Pi/OMP extension that forwards visible conversation text and tool traffic to `curator ingest`.

```
omp -e plugin/index.ts          # or install as an extension
PI_CURATOR_BIN=/path/to/curator # defaults to `curator` on PATH
```

Behavior:

- Memory is never created without consent. At `session_start` a repository in `pending_consent` state triggers `ctx.ui.confirm`. Without a UI, or after a recorded decline, nothing is captured and a warning is logged.
- Only visible `text` blocks are captured. Thinking blocks are never read.
- Events go through a bounded in-memory queue. Overflow, repeated curator failure, and undelivered events at shutdown become explicit gaps, which curator journals (`curator gaps`).
- Shutdown waits at most 5 s for delivery.
- Curator redacts, applies path denials, and decides durability. The plugin counts an event as delivered only after curator reports `durable` or `duplicate`.
- Native `memory_search` and exact paginated `memory_read` are available in consented repositories. Task-conditioned prefetch is enabled by default with a complete 500-estimated-token output budget (2,000 UTF-8 bytes). Set `PI_CURATOR_PREFETCH_BUDGET=0` to disable automatic prefetch without disabling native tools; values 64–8192 override the budget. Invalid values warn and disable prefetch. Prefetch supplies excerpts, not automatic exact reads; the agent can open source evidence afterward.
- The system prompt gets only static recall guidance. `PI_CURATOR_STARTUP_DECISIONS=N` (1 to 10) delivers the N newest user decisions as ordinary untrusted custom-message content. Unset, empty or non-numeric means off. If task prefetch is also enabled, combined content must fit its budget; startup content that would overflow is omitted with a warning.
- Capture and native tools remain bound to the consented repository root, including valid subdirectories. Foreign repository contexts are denied.
- stdout and stderr share a 1 MiB byte limit. Overflow kills the child and fails observably; command errors do not reflect child payloads.

Development (`bun` required; `tsc` needs `@types/node` resolvable):

```
bun test
tsc --noEmit -p .
```

`test/plugin.test.ts` runs against `curator/bin/curator` (`cd curator && make build`) and is skipped when the binary is absent.
