package cli

import (
	"fmt"
	"path/filepath"
	"time"

	"pi-curator/curator/internal/journal"
	"pi-curator/curator/internal/lifecycle"
	"pi-curator/curator/internal/lock"
	"pi-curator/curator/internal/onboard"
)

// readyStore resolves the current repository's store; it fails unless memory
// is initialized and effectively excluded from version control.
func (e env) readyStore(cwd, op string) (onboard.Status, int) {
	dir, err := resolveCwd(cwd)
	if err != nil {
		return onboard.Status{}, e.failure("resolve directory", err)
	}
	st, err := onboard.Inspect(dir)
	if err != nil {
		return onboard.Status{}, e.failure("inspect repository", err)
	}
	if st.State != onboard.StateReady {
		return onboard.Status{}, e.failure(op, fmt.Errorf("memory is not ready (state %s)", st.State))
	}
	return st, exitOK
}

// mutate runs pin, unpin, archive, restore or supersede on one memory ID.
func (e env) mutate(action string, args []string) int {
	fs := e.flags(action)
	cwd := cwdFlag(fs)
	by := ""
	reason := fs.String("reason", "", "why (recorded in the lifecycle journal)")
	if action == "supersede" {
		fs.StringVar(&by, "by", "", "ID of the memory that replaces this one (required)")
	}
	if !e.parsePositional(fs, args) {
		return exitUsage
	}
	if fs.NArg() != 1 {
		return e.usageError(action + " needs exactly one memory ID")
	}
	if action == "supersede" && by == "" {
		return e.usageError("supersede requires --by ID")
	}
	st, code := e.readyStore(*cwd, action)
	if code != exitOK {
		return code
	}
	var res lifecycle.Result
	err := lock.With(filepath.Join(st.Store, lock.FileName), lockTimeout, func(h *lock.Held) error {
		var aerr error
		res, aerr = lifecycle.Apply(h, journal.New(st.Store), st.Store, action, fs.Arg(0), by, *reason, time.Now())
		return aerr
	})
	if err != nil {
		return e.failure(action, err)
	}
	return e.json(res)
}

// list prints memories in a lifecycle state, one JSON object per line.
func (e env) list(args []string) int {
	fs := e.flags("list")
	cwd := cwdFlag(fs)
	state := fs.String("state", lifecycle.StateActive, "active, superseded, archived or all")
	if !e.parse(fs, args) {
		return exitUsage
	}
	st, code := e.readyStore(*cwd, "list")
	if code != exitOK {
		return code
	}
	entries, err := lifecycle.List(journal.New(st.Store), *state)
	if err != nil {
		return e.failure("list", err)
	}
	for _, en := range entries {
		if code := e.json(en); code != exitOK {
			return code
		}
	}
	return exitOK
}

// show prints one memory with lifecycle history and cited raw events.
func (e env) show(args []string) int {
	fs := e.flags("show")
	cwd := cwdFlag(fs)
	if !e.parsePositional(fs, args) {
		return exitUsage
	}
	if fs.NArg() != 1 {
		return e.usageError("show needs exactly one memory ID")
	}
	st, code := e.readyStore(*cwd, "show")
	if code != exitOK {
		return code
	}
	det, err := lifecycle.Show(journal.New(st.Store), fs.Arg(0))
	if err != nil {
		return e.failure("show", err)
	}
	return e.json(det)
}
