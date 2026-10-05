package record

// State is the lifecycle state of one memory ID after replay.
type State struct {
	Pinned       bool
	Archived     bool
	SupersededBy string
}

// Replay folds lifecycle records, in journal order, into per-target state.
// A record ID applied more than once has no further effect, so replaying a
// journal that contains retried duplicates is idempotent.
func Replay(records []Lifecycle) map[string]State {
	seen := make(map[string]bool, len(records))
	states := make(map[string]State)
	for _, r := range records {
		if seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		s := states[r.Target]
		switch r.Action {
		case ActionPin:
			s.Pinned = true
		case ActionUnpin:
			s.Pinned = false
		case ActionArchive:
			s.Archived = true
		case ActionRestore:
			s.Archived = false
		case ActionSupersede:
			s.SupersededBy = r.SupersededBy
		}
		states[r.Target] = s
	}
	return states
}
