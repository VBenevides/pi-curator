package cli

import (
	"path/filepath"

	"pi-curator/curator/internal/journal"
	"pi-curator/curator/internal/lock"
	"pi-curator/curator/internal/memory"
)

// maintain builds pending episodes and republishes the memory views under the
// exclusive lock. --flush also closes each session's trailing open group.
// --dry-run reports the same summary without writing or locking.
func (e env) maintain(args []string) int {
	fs := e.flags("maintain")
	cwd := cwdFlag(fs)
	flush := fs.Bool("flush", false, "close open episodes (use at session end)")
	dry := fs.Bool("dry-run", false, "report what would change; write nothing")
	if !e.parse(fs, args) {
		return exitUsage
	}
	st, code := e.readyStore(*cwd, "maintain")
	if code != exitOK {
		return code
	}
	opts := memory.Options{Flush: *flush}
	if *dry {
		sum, err := memory.Plan(journal.New(st.Store), opts)
		if err != nil {
			return e.failure("maintain --dry-run", err)
		}
		return e.json(sum)
	}
	var sum memory.Summary
	err := lock.With(filepath.Join(st.Store, lock.FileName), lockTimeout, func(h *lock.Held) error {
		var rerr error
		sum, rerr = memory.Rebuild(h, journal.New(st.Store), st.Store, opts)
		return rerr
	})
	if err != nil {
		return e.failure("maintain", err)
	}
	return e.json(sum)
}
