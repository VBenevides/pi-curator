// Package cli implements the curator command-line interface.
package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"pi-curator/curator/internal/lock"
)

const usage = `Usage: curator <command> [flags]

Commands:
  version, --version     Show version
  status [--cwd DIR]     Print repository onboarding state as JSON (read-only)
  init --consent         Create .curator and ignore it (after user consent)
  ingest [--cwd DIR]     Journal captured events: JSON request on stdin, JSON result on stdout
  maintain [--flush] [--dry-run]
                         Build episodes and republish .curator/memory views
  pin|unpin|archive|restore [--cwd DIR] ID
  supersede --by NEWID [--cwd DIR] ID
                         Explicit lifecycle changes (journaled, locked)
  list [--state S]       Print memories by state (read-only)
  show ID                Print a memory, its history and cited raw events (read-only)
  state --key K --source EVENT_ID [--branch B|--repository-wide]
                         Declare scoped state from existing redacted evidence
  search [--file PATH] [--decisions] [--limit N] [--snippet N] [WORDS...]
                         Rank earlier messages against the words, or events that mention a file, or user decisions; bounded output (read-only)
  memory-search --query Q [--budget N]
                         Search repository history through the agent tool interface
  read [--indexed] [--cursor N] [--budget N] EVENT_ID [EVENT_ID ...]
                         Exact stored UTF-8 pages; 1..5 IDs, byte cursor, total JSON budget 256..8192
  index [--rebuild]       Incrementally index redacted history in a disposable SQLite/FTS sidecar
  feedback --label L --source S ID  Record explicit feedback (or --list [ID])
  gaps                   List reported capture gaps and whether each was recovered (read-only)
  recover                Quarantine an incomplete journal tail so ingestion can resume
`

// Exit statuses.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

type env struct {
	stdin          io.Reader
	stdout, stderr io.Writer
}

// Run executes a command and returns its process exit status:
// 0 for success, 1 for operational failures, and 2 for invalid usage.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer, version string) int {
	e := env{stdin: stdin, stdout: stdout, stderr: stderr}
	if len(args) == 0 {
		return e.print(usage)
	}
	switch args[0] {
	case "help", "-h", "--help":
		if len(args) > 1 {
			return e.usageError("expected a single command or flag")
		}
		return e.print(usage)
	case "version", "--version":
		if len(args) > 1 {
			return e.usageError("expected a single command or flag")
		}
		return e.print(fmt.Sprintf("curator %s\n", version))
	case "status":
		return e.status(args[1:])
	case "init":
		return e.initRepo(args[1:])
	case "ingest":
		return e.ingestCmd(args[1:])
	case "maintain":
		return e.maintain(args[1:])
	case "pin", "unpin", "archive", "restore", "supersede":
		return e.mutate(args[0], args[1:])
	case "list":
		return e.list(args[1:])
	case "show":
		return e.show(args[1:])
	case "state":
		return e.stateCmd(args[1:])
	case "search":
		return e.searchCmd(args[1:])
	case "memory-search":
		return e.toolSearch(args[1:])
	case "read":
		return e.readEvents(args[1:])
	case "index":
		return e.indexCmd(args[1:])
	case "feedback":
		return e.feedbackCmd(args[1:])
	case "gaps":
		return e.gaps(args[1:])
	case "recover":
		return e.recoverJournal(args[1:])
	}
	return e.usageError(fmt.Sprintf("unknown command %q; use --help", args[0]))
}

func (e env) print(s string) int {
	if _, err := io.WriteString(e.stdout, s); err != nil {
		fmt.Fprintf(e.stderr, "curator: write output: %v\n", err)
		return exitFailure
	}
	return exitOK
}

func (e env) usageError(msg string) int {
	fmt.Fprintf(e.stderr, "curator: %s\n", msg)
	return exitUsage
}

func (e env) failure(op string, err error) int {
	fmt.Fprintf(e.stderr, "curator: %s: %v\n", op, err)
	return exitFailure
}

// json writes one compact JSON document and a newline.
func (e env) json(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return e.failure("encode output", err)
	}
	return e.print(string(b) + "\n")
}

// flags builds a flag set that reports parse errors to stderr.
func (e env) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("curator "+name, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	return fs
}

// parse parses flags and rejects positional arguments.
func (e env) parse(fs *flag.FlagSet, args []string) (ok bool) {
	if err := fs.Parse(args); err != nil {
		return false
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(e.stderr, "curator %s: unexpected argument %q\n", fs.Name(), fs.Arg(0))
		return false
	}
	return true
}

// parsePositional parses flags and permits positional arguments, which must
// follow the flags (Go flag parsing stops at the first positional).
func (e env) parsePositional(fs *flag.FlagSet, args []string) bool {
	return fs.Parse(args) == nil
}

// lockTimeout bounds how long a mutating command waits for the memory lock.
var lockTimeout = lock.DefaultTimeout
