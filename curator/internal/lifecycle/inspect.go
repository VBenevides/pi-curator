package lifecycle

import (
	"fmt"

	"pi-curator/curator/internal/journal"
	"pi-curator/curator/internal/memory"
	"pi-curator/curator/internal/record"
)

// Listing states.
const (
	StateActive     = "active"
	StateSuperseded = "superseded"
	StateArchived   = "archived"
	StateAll        = "all"
)

// Entry is one memory with its replayed lifecycle state.
type Entry struct {
	Memory       *record.Memory `json:"memory"`
	State        string         `json:"state"`
	Pinned       bool           `json:"pinned"`
	SupersededBy string         `json:"superseded_by,omitempty"`
}

// Detail is the full provenance view of one memory.
type Detail struct {
	Entry
	History []record.Lifecycle `json:"history"`
	Events  []*record.Event    `json:"events"`
	// MissingRefs lists source references that no longer resolve.
	MissingRefs []string `json:"missing_refs,omitempty"`
}

func entries(d *memory.Data) []Entry {
	states := record.Replay(d.Lifecycle)
	var out []Entry
	memory.Visit(d, func(m *record.Memory) bool {
		st := states[m.ID]
		e := Entry{Memory: m, State: StateActive, Pinned: st.Pinned, SupersededBy: st.SupersededBy}
		switch {
		case st.Archived:
			e.State = StateArchived
		case st.SupersededBy != "":
			e.State = StateSuperseded
			m.Status = memory.StatusSuperseded
		}
		if st.Pinned {
			m.Retention = memory.RetentionPinned
		}
		out = append(out, e)
		return true
	})
	return out
}

// List returns memories in the given state ("all" for every state). It reads
// the journal without locking or writing.
func List(j *journal.Journal, state string) ([]Entry, error) {
	switch state {
	case StateActive, StateSuperseded, StateArchived, StateAll:
	default:
		return nil, fmt.Errorf("unknown state %q (use active, superseded, archived or all)", state)
	}
	d, err := memory.Load(j)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, e := range entries(d) {
		if state == StateAll || e.State == state {
			out = append(out, e)
		}
	}
	return out, nil
}

// Show returns one memory with lifecycle history and the raw source events it
// cites. Event content is returned as data; nothing in it is interpreted.
func Show(j *journal.Journal, id string) (*Detail, error) {
	d, err := memory.Load(j)
	if err != nil {
		return nil, err
	}
	for _, e := range entries(d) {
		if e.Memory.ID != id {
			continue
		}
		det := &Detail{Entry: e}
		for _, l := range d.Lifecycle {
			if l.Target == id || l.SupersededBy == id {
				det.History = append(det.History, l)
			}
		}
		for _, ref := range e.Memory.SourceRefs {
			kind, rid, err := record.ParseRef(ref)
			if err != nil {
				return nil, err
			}
			if kind != record.RefEvent {
				continue
			}
			if ev, ok := d.Events[rid]; ok {
				det.Events = append(det.Events, ev)
			} else {
				det.MissingRefs = append(det.MissingRefs, ref)
			}
		}
		return det, nil
	}
	return nil, fmt.Errorf("unknown memory %q", id)
}
