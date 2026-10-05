package cli

import (
	"path/filepath"

	"pi-curator/curator/internal/ingest"
	"pi-curator/curator/internal/journal"
	"pi-curator/curator/internal/lock"
)

// gaps lists capture gaps reported by the plugin and whether each has been
// recovered. Read-only.
func (e env) gaps(args []string) int {
	fs := e.flags("gaps")
	cwd := cwdFlag(fs)
	if !e.parse(fs, args) {
		return exitUsage
	}
	st, code := e.readyStore(*cwd, "gaps")
	if code != exitOK {
		return code
	}
	list, err := ingest.Gaps(journal.New(st.Store))
	if err != nil {
		return e.failure("gaps", err)
	}
	for _, g := range list {
		if code := e.json(g); code != exitOK {
			return code
		}
	}
	return exitOK
}

// recoverJournal quarantines an incomplete journal tail under the lock so
// ingestion can resume. Completed records are never altered.
func (e env) recoverJournal(args []string) int {
	fs := e.flags("recover")
	cwd := cwdFlag(fs)
	if !e.parse(fs, args) {
		return exitUsage
	}
	st, code := e.readyStore(*cwd, "recover")
	if code != exitOK {
		return code
	}
	var quarantined string
	err := lock.With(filepath.Join(st.Store, lock.FileName), lockTimeout, func(h *lock.Held) error {
		var rerr error
		quarantined, rerr = journal.New(st.Store).Recover(h)
		return rerr
	})
	if err != nil {
		return e.failure("recover", err)
	}
	return e.json(map[string]any{"recovered": quarantined != "", "quarantine": quarantined})
}
