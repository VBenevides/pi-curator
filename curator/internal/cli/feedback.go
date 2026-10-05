package cli

import (
	"path/filepath"
	"time"

	"pi-curator/curator/internal/feedback"
	"pi-curator/curator/internal/journal"
	"pi-curator/curator/internal/lock"
	"pi-curator/curator/internal/redact"
)

// feedbackCmd records an explicit label for an episode, or lists labels.
func (e env) feedbackCmd(args []string) int {
	fs := e.flags("feedback")
	cwd := cwdFlag(fs)
	label := fs.String("label", "", "useful, misleading, outdated, outcome_success or outcome_failure")
	source := fs.String("source", "", "user_correction, task_outcome or benchmark")
	note := fs.String("note", "", "optional note (redacted, max 500 characters)")
	list := fs.Bool("list", false, "list recorded feedback instead of adding")
	if !e.parsePositional(fs, args) {
		return exitUsage
	}
	st, code := e.readyStore(*cwd, "feedback")
	if code != exitOK {
		return code
	}
	j := journal.New(st.Store)
	if *list {
		if fs.NArg() > 1 {
			return e.usageError("feedback --list takes at most one episode or memory ID")
		}
		target := ""
		if fs.NArg() == 1 {
			target = fs.Arg(0)
		}
		items, err := feedback.List(j, target)
		if err != nil {
			return e.failure("feedback", err)
		}
		for _, it := range items {
			if code := e.json(it); code != exitOK {
				return code
			}
		}
		return exitOK
	}
	if fs.NArg() != 1 || *label == "" || *source == "" {
		return e.usageError("feedback needs --label L --source S ID")
	}
	policy, err := redact.Load(st.Store)
	if err != nil {
		return e.failure("feedback", err)
	}
	var res feedback.Result
	err = lock.With(filepath.Join(st.Store, lock.FileName), lockTimeout, func(h *lock.Held) error {
		var aerr error
		res, aerr = feedback.Add(h, j, policy, fs.Arg(0), *label, *source, *note, time.Now())
		return aerr
	})
	if err != nil {
		return e.failure("feedback", err)
	}
	return e.json(res)
}
