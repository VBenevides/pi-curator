package cli

import (
	"strings"

	"pi-curator/curator/internal/index"
	"pi-curator/curator/internal/journal"
	"pi-curator/curator/internal/memory"
	"pi-curator/curator/internal/record"
	"pi-curator/curator/internal/search"
)

// readEvents returns exact stored UTF-8 pages, with one budget for the batch.
func (e env) readEvents(args []string) int {
	fs := e.flags("read")
	cwd := cwdFlag(fs)
	cursor := fs.Int("cursor", 0, "UTF-8 byte cursor returned by a previous page")
	budget := fs.Int("budget", 800, "total serialized output tokens (ceil UTF-8 bytes/4)")
	indexed := fs.Bool("indexed", false, "use verified indexed source locations")
	if !e.parsePositional(fs, args) {
		return exitUsage
	}
	if fs.NArg() < 1 || fs.NArg() > 5 || *cursor < 0 || (*cursor != 0 && fs.NArg() != 1) || *budget < 256 || *budget > 8192 {
		return e.usageError("read needs 1..5 event IDs, a single-event byte cursor, and budget 256..8192")
	}
	st, code := e.readyStore(*cwd, "read")
	if code != exitOK {
		return code
	}
	out, err := evidencePages(st.Store, fs.Args(), *cursor, *budget, *indexed)
	if err != nil {
		return e.failure("read", err)
	}
	return e.print(string(out) + "\n")
}

func (e env) toolSearch(args []string) int {
	fs := e.flags("memory-search")
	cwd := cwdFlag(fs)
	query := fs.String("query", "", "behavioral terms or exact identifiers")
	budget := fs.Int("budget", 250, "estimated output tokens")
	engine := fs.String("engine", "legacy", "candidate engine: legacy, fts, hybrid, episodes or state")
	limit := fs.Int("limit", 5, "candidate limit, 1..50")
	format := fs.String("format", "compact", "compact budgeted text or bounded diagnostic json")
	history := fs.Bool("history", false, "include inactive state declarations within the current scope")
	if !e.parse(fs, args) {
		return exitUsage
	}
	if strings.TrimSpace(*query) == "" || *budget < 64 || *budget > 8192 {
		return e.usageError("memory-search needs --query and budget 64..8192")
	}
	if *limit < 1 || *limit > 50 || (*format != "compact" && *format != "json") {
		return e.usageError("limit must be 1..50 and format compact or json")
	}
	st, code := e.readyStore(*cwd, "memory-search")
	if code != exitOK {
		return code
	}
	if *engine == "fts" || *engine == "hybrid" {
		idx, err := index.Existing(st.Store)
		if err != nil {
			return e.failure("memory-search", err)
		}
		defer idx.Close()
		var res search.Result
		if *engine == "hybrid" {
			res, err = idx.Hybrid(*query, *limit)
		} else {
			res, err = idx.Search(*query, *limit)
		}
		if err != nil {
			return e.failure("memory-search", err)
		}
		if *format == "json" {
			return e.json(res)
		}
		return e.print(search.Compact(res, *budget))
	}
	if *engine != "legacy" && *engine != "episodes" && *engine != "state" {
		return e.usageError("engine must be legacy, fts, hybrid, episodes or state")
	}
	d, err := memory.Load(journal.New(st.Store))
	if err != nil {
		return e.failure("memory-search", err)
	}
	opts := search.Options{Terms: []string{*query}, Limit: *limit}
	var res search.Result
	if *engine == "state" {
		branch, branchErr := currentBranch(st.Root)
		if branchErr != nil {
			return e.failure("memory-search", branchErr)
		}
		res, err = search.State(d, opts, record.Scope{Repository: st.RepoID, Branch: branch}, *history)
	} else if *engine == "episodes" {
		res, err = search.Episodes(d, opts)
	} else {
		res, err = search.Run(d, opts)
	}
	if err != nil {
		return e.failure("memory-search", err)
	}
	if *format == "json" {
		return e.json(res)
	}
	return e.print(search.Compact(res, *budget))
}
