package cli

import (
	"pi-curator/curator/internal/journal"
	"pi-curator/curator/internal/memory"
	"pi-curator/curator/internal/search"
)

// searchCmd prints a ranked, bounded answer for a few query words, optionally
// limited to events that mention a file or to user decisions. It reads the
// journal without locking or writing.
func (e env) searchCmd(args []string) int {
	fs := e.flags("search")
	cwd := cwdFlag(fs)
	file := fs.String("file", "", "only events that mention this file (tool calls included); words then only rank")
	decisions := fs.Bool("decisions", false, "only user messages that state a decision or rule; newest first without words")
	limit := fs.Int("limit", search.DefaultLimit, "most hits to return")
	snippet := fs.Int("snippet", 0, "most characters per hit (default 400; a decision hit gets 1000)")
	if !e.parsePositional(fs, args) {
		return exitUsage
	}
	if fs.NArg() == 0 && *file == "" && !*decisions {
		return e.usageError("search needs at least one word, --file or --decisions")
	}
	st, code := e.readyStore(*cwd, "search")
	if code != exitOK {
		return code
	}
	d, err := memory.Load(journal.New(st.Store))
	if err != nil {
		return e.failure("search", err)
	}
	res, err := search.Run(d, search.Options{Terms: fs.Args(), File: *file, DecisionsOnly: *decisions, Limit: *limit, SnippetRunes: *snippet})
	if err != nil {
		return e.failure("search", err)
	}
	return e.json(res)
}
