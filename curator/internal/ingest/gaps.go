package ingest

import (
	"fmt"
	"strings"

	"pi-curator/curator/internal/journal"
	"pi-curator/curator/internal/record"
)

// GapStatus is one reported capture gap and whether the event has since been
// recovered into the journal.
type GapStatus struct {
	SessionID  string `json:"session_id"`
	EventID    string `json:"event_id"`
	ReportedAt string `json:"reported_at_utc"`
	Recovered  bool   `json:"recovered"`
}

// Gaps lists every reported gap. A gap is recovered once an event with the
// same ID exists, so replaying host history closes it idempotently; the rest
// stay explicit. It reads the journal without locking or writing.
func Gaps(j *journal.Journal) ([]GapStatus, error) {
	events := map[string]bool{}
	var gaps []GapStatus
	rep, err := j.Scan(func(e journal.Entry) error {
		switch r := e.Rec.(type) {
		case *record.Event:
			events[r.SessionID+"/"+r.ID] = true
		case *record.Progress:
			if r.Stage != GapStage {
				return nil
			}
			session, event, ok := strings.Cut(r.Cursor, "/")
			if !ok {
				return fmt.Errorf("gap %s has malformed cursor %q", r.ID, r.Cursor)
			}
			gaps = append(gaps, GapStatus{SessionID: session, EventID: event, ReportedAt: r.CreatedAtUTC})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read journal: %w", err)
	}
	if !rep.Healthy() {
		return nil, fmt.Errorf("journal is damaged (%d malformed records, incomplete tail: %t); gap list may be incomplete",
			len(rep.Malformed), rep.Tail != nil)
	}
	for i := range gaps {
		gaps[i].Recovered = events[gaps[i].SessionID+"/"+gaps[i].EventID]
	}
	return gaps, nil
}
