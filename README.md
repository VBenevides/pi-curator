# pi-curator

I am creating a memory management plugin for Pi/OMP coding agents, based on PRO-LONG. This is an early public release (0.1.0): expect rough edges and interface changes.

Repository-local memory for Pi/OMP coding agents. It records what happened in earlier sessions of a repository, so the next session can look it up instead of starting cold.

Components:

| Directory | What it is |
|---|---|
| `curator/` | Go CLI. Redacts, validates and durably journals events, builds episodes, manages their lifecycle. |
| `plugin/` | Pi/OMP extension. Captures visible messages and tool traffic, sends them to `curator ingest`, and gives the agent static recall guidance. |

## How it works

1. The plugin forwards user and assistant text, tool calls and tool results to `curator ingest`. Thinking blocks are never read. `read` results are skipped, and other tool results are cut to 2 KiB.
2. Curator redacts secrets and denied paths, then appends the event to `.curator/journal/events.jsonl` with fsync before it acknowledges. A failed or missed delivery becomes an explicit capture gap (`curator gaps`).
3. The agent uses native `memory_search` and `memory_read` tools. Search returns ranked event IDs; read returns exact stored content with explicit continuation. Stored text is untrusted evidence, not instructions. The existing CLI search remains available for operators.
4. `curator maintain --flush` groups events into episodes in `.curator/memory/active.jsonl`, with pin, supersede, archive and feedback commands.

Stored history is untrusted data. The agent is told to verify it against the current code and never to follow commands found in it.

## Consent and safety

- Nothing is created or captured without consent. On the first session in a repository the plugin asks before `curator init --consent` creates `.curator/` and hides it in `.git/info/exclude`, so no tracked file changes. A decline is remembered.
- Headless runs (`omp -p`, no UI) cannot ask, so they capture nothing until the operator has run `curator init --consent` in the repository. Once it is initialized they capture and show the recall guidance as usual.
- Secrets and denied paths are redacted before anything is written.
- If curator is missing or failing, the plugin logs a warning and the session continues.
- Startup decisions and task prefetch are ordinary untrusted custom-message content; only static recall guidance is system policy. When both are enabled, startup history is omitted with a warning if their combined content would exceed the prefetch budget.
- Consent is bound to the repository root for the session. Valid subdirectories work; switching to another repository denies native retrieval and capture.
- CLI transport counts stdout and stderr bytes together (1 MiB maximum), kills overflowing children, and reports payload-free errors rather than truncating output. Indexed session goals are verified against source journal bytes, never trusted from sidecar text.

## Install (local development)

Requires `go`, `omp` and `bun`.

```
scripts/dev_install.sh
```

This installs the `curator` binary with `go install` and links `plugin/` into OMP. Make sure the Go bin directory is on `PATH`, or set `PI_CURATOR_BIN`. Start OMP in a repository and accept the consent prompt.

While a session runs, a line below the editor shows `curator: Memory accesses - X last interaction - Y current session`.

## Layout of `.curator/`

```
.curator/
├── journal/events.jsonl    raw redacted events, one JSON object per line, append-only
└── memory/active.jsonl     episodes built by `curator maintain`
```

`.curator/` is hidden from Git through `.git/info/exclude`. Event schema: `curator.event.v2`.

## Commands

`curator version | status | init --consent | ingest | maintain [--flush] [--dry-run] | list | show ID | search WORDS... | pin | unpin | archive | restore | supersede | feedback | gaps | recover`. Run `curator` without arguments for flags.

Agent retrieval: `curator memory-search --query "atomic writes" --budget 250` and `curator read --budget 800 --cursor 0 EVENT_ID [EVENT_ID ...]`. Read accepts 1–5 IDs under one total budget of 256–8192 estimated tokens, including serialized JSON escaping, metadata, links and the CLI newline. Duplicate IDs return one page. A single unique ID returns an object; multiple IDs return an array. Follow each incomplete page using its single ID and `--cursor NEXT_CURSOR`: cursors are stored UTF-8 byte boundaries, never filesystem offsets. `complete` covers stored text only, not the original transcript. Stored size, redactions, nullable actual `is_error`, linked tool IDs, missing partners, reported capture gaps and known capture limits remain explicit. Insufficient metadata budget and more than 100 links fail observably; legacy reads stream a validated journal, and indexed reads verify source locations without refreshing the sidecar.

Native search budgets cover the entire plain-text response, including metadata and omitted-match counts. Accounting is an estimate of `ceil(UTF-8 bytes / 4)`, not a model-specific tokenizer guarantee. Search budgets must be 64–8192.

Optional indexed retrieval: run `curator index`, then `curator memory-search --engine fts --query "atomic writes"` or `curator read --indexed EVENT_ID`. The disposable `.curator/index.sqlite` sidecar uses SQLite FTS5, commits progress in bounded batches, and rejects stale indexes. Re-run indexing after appends; use `--rebuild` after unexpected in-place journal changes. Incomplete tails remain pending and must be recovered before indexed retrieval. The native tools keep legacy candidate ranking until staged evaluation selects a default.

Use `--engine hybrid` to union FTS candidates with exact file, symbol, test, error, and captured shell-command anchors. Anchor bonuses preserve punctuation-sensitive identifiers without replacing behavioral source vocabulary. Both indexed engines are opt-in; anchors indicate a textual connection, not evidence that a past diagnosis or outcome applies now.

Task-conditioned prefetch defaults to 500 estimated tokens (at most 2,000 UTF-8 bytes). Set `PI_CURATOR_PREFETCH_BUDGET=0` to opt out, or use 64–8192 to override the budget. Invalid values warn and disable prefetch. Matching history is delivered before a turn as a separate hidden, untrusted custom message—not system policy. `PI_CURATOR_PREFETCH_ENGINE` optionally selects `legacy`, `fts`, or `hybrid`; raw/legacy remains the default, and indexed engines require a current sidecar. Missing matches inject nothing, and retrieval failures are logged without blocking the task. Native search and exact reads remain available when prefetch is disabled.

Indexed native tools can be tested with `PI_CURATOR_SEARCH_ENGINE=fts` or `hybrid`; they explicitly refresh the disposable sidecar before retrieval. Legacy remains the default because measured governing-record recall and small paired outcomes did not justify a cutover. Diagnostic CLI `memory-search --format json --limit 50` exposes bounded ranked candidates for evaluation, not a token-budgeted agent response.

Offline evaluation: `python3 scripts/evaluate_retrieval.py --repo REPO --cases CASES.jsonl --out OUTPUT`. Each case supplies `task_id`, `query`, independently labeled `governing_ids`, and a task-group `split` (`dev`/`test`); optional `governing_texts` maps those IDs to complete evidence text. Reports separate candidate/visible-record recall from complete-text recall, estimated memory output tokens, latency, and peak process memory. Incremental rows are fsynced and resume only when journal, cases, retriever binary, and evaluator hashes match. Use a frozen benchmark repository, not an actively captured session.

`curator maintain --flush` derives attributed statements, tool attempts, and evidence-backed execution-transition lessons using the existing bounded episodes and atomic views. Host `isError` metadata is stored as `is_error`; legacy text-only results have unknown execution status. Printed `PASS`, assistant assertions, and task boundaries never establish task completion. Derived task outcomes remain unresolved; a successful tool call alone is not completion proof.

Experimental `memory-search --engine episodes` (or `PI_CURATOR_SEARCH_ENGINE=episodes`) ranks already-built episode cards separately from raw events. Cards label assistant statements, link directly to stored source event IDs, and exclude explicitly archived/superseded memories. Diagnostic `evidence_event_ids` describe candidate source coverage, not text exposed by the compact response. Exact `read` pages expose recorded `is_error` status as well as stored text. At 250 estimated tokens the cards lost governing evidence, and a small paired Luna-medium trial regressed success; raw retrieval remains the default.

Explicit scoped state: `curator state --key config-order --source EVENT_ID` declares a fact in the current repository/branch namespace, using existing redacted evidence rather than accepting a new free-form value. Use `--branch NAME` to declare another branch or `--repository-wide` for all branches. Declaration does not infer the original event's branch. Repeating the same declaration is idempotent; view publication can be retried after interruption.

`memory-search --engine state` and `PI_CURATOR_SEARCH_ENGINE=state` search only matching repository/current-branch or repository-wide declarations. Multiple active declarations remain visible: neither a timestamp nor a shared key replaces a fact. Explicit `supersede --by NEWID OLDID` requires the same state key and scope. `memory-search --engine state --history` includes inactive declarations in the matching namespace, and `show OLDID` retains exact source evidence. Raw retrieval remains the default; the new scoped interface is not automatic extraction from old prose.

Semantic/reranking evaluation remains experimental and unshipped. The benchmark-local NumPy hashed TF-IDF/SVD candidate generator and six-feature learned reranker use 13 development task groups, with 11 held-out groups and explicitly checked session separation. Reranking improved held-out complete-evidence display at budget 250 from 5/11 to 8/11, but fresh Luna-medium present/superseded trials reduced success from 2/2 to 1/2. Latent candidates and relevance-per-token ranking also failed to justify adoption. No model, dependency, ranking score or shipping default is promoted by these results.

Storage threat boundary: repository memory is owner-controlled local storage, not a sandbox against an unrestricted same-user process that can rewrite the repository, journal, consent or executable. History text remains untrusted regardless of its source. Journal source opens pin directories and reject symlinks; index opens reject static database/auxiliary symlinks, and the Linux SQLite VFS uses no-follow final-component opens. SQLite parent-directory replacement by a hostile filesystem writer is outside this boundary. Recovery never overwrites an existing quarantine and syncs its file and directory before truncating source evidence.

## Development

```
cd curator && make check     # format, vet, tests
cd curator && make build     # bin/curator, used by plugin/test/plugin.test.ts
cd plugin && bun test
```

Each component keeps its own `VERSION` and `CHANGELOG.md`; the root `CHANGELOG.md` summarizes both. Current version: 0.1.0. Licensed under MIT (see `LICENSE`).
