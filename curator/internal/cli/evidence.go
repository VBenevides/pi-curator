package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"pi-curator/curator/internal/index"
	"pi-curator/curator/internal/journal"
	"pi-curator/curator/internal/record"
)

type evidencePage struct {
	EventID         string        `json:"event_id"`
	SessionID       string        `json:"session_id"`
	Category        string        `json:"category"`
	Created         string        `json:"created_at_utc"`
	ToolName        string        `json:"tool_name,omitempty"`
	CallID          string        `json:"call_id,omitempty"`
	Source          record.Source `json:"source"`
	IsError         *bool         `json:"is_error"`
	Redactions      int           `json:"redactions"`
	Content         string        `json:"content"`
	Cursor          int           `json:"cursor"`
	Next            int           `json:"next_cursor"`
	Complete        bool          `json:"complete"`
	StoredBytes     int           `json:"stored_bytes"`
	Linked          []string      `json:"linked_events"`
	LinksComplete   bool          `json:"links_complete"`
	MissingPartner  bool          `json:"missing_partner"`
	CaptureGaps     int           `json:"capture_gaps"`
	CaptureFidelity string        `json:"capture_fidelity"`
	Untrusted       bool          `json:"untrusted"`
	Accounting      string        `json:"accounting"`
}

// scanEvidence uses the journal's secure opener and bounded record reader. Even
// a matching event cannot turn a malformed or interrupted journal into success.
func scanEvidence(store string, visit func(journal.Entry) error) error {
	rep, err := journal.New(store).Scan(visit)
	if err != nil {
		return err
	}
	if !rep.Healthy() {
		return fmt.Errorf("journal is unhealthy: %d malformed records, overrun=%t, incomplete_tail=%t; recover before reading", len(rep.Malformed), rep.MalformedOverrun, rep.Tail != nil)
	}
	return nil
}

func evidencePages(store string, ids []string, cursor, budget int, indexed bool) ([]byte, error) {
	events := make(map[string]*record.Event, len(ids))
	order := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := events[id]; !ok {
			events[id] = nil
			order = append(order, id)
		}
	}
	var idx *index.Index
	if indexed {
		var err error
		idx, err = index.Existing(store)
		if err != nil {
			return nil, err
		}
		defer idx.Close()
		for _, id := range order {
			ev, err := idx.Read(id)
			if err != nil {
				return nil, err
			}
			events[id] = ev
		}
	} else {
		if err := scanEvidence(store, func(entry journal.Entry) error {
			if ev, ok := entry.Rec.(*record.Event); ok {
				if _, wanted := events[ev.ID]; wanted {
					events[ev.ID] = ev
				}
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	pages := make([]evidencePage, 0, len(order))
	for _, id := range order {
		ev := events[id]
		if ev == nil {
			return nil, fmt.Errorf("unknown event %q", id)
		}
		if !utf8.ValidString(ev.Content) {
			return nil, fmt.Errorf("event %q contains invalid UTF-8", id)
		}
		if cursor > len(ev.Content) || (cursor < len(ev.Content) && !utf8.RuneStart(ev.Content[cursor])) {
			return nil, fmt.Errorf("cursor is not a stored UTF-8 boundary for %q", id)
		}
		fidelity := "exact stored text; original completeness unknown"
		if ev.Category == record.CategoryToolResult && strings.Contains(ev.Content, "\n[truncated: kept ") && strings.HasSuffix(ev.Content, " bytes]") {
			fidelity = "exact stored text; original tool result capture limited to 2048 bytes plus marker; omitted original text unavailable"
		}
		p := evidencePage{EventID: id, SessionID: ev.SessionID, Category: ev.Category, Created: ev.CreatedAtUTC, IsError: ev.IsError, Redactions: ev.Redactions, Cursor: cursor, Next: cursor, StoredBytes: len(ev.Content), Linked: []string{}, LinksComplete: true, CaptureFidelity: fidelity}
		p.Untrusted = true
		p.Accounting = "ceil(serialized UTF-8 bytes including newline/4); total batch budget"
		p.ToolName, p.CallID, p.Source = ev.ToolName, ev.CallID, ev.Source
		callSeen, resultSeen := ev.Category == record.CategoryToolCall, ev.Category == record.CategoryToolResult
		if err := scanEvidence(store, func(entry journal.Entry) error {
			if other, ok := entry.Rec.(*record.Event); ok && ev.CallID != "" && other.CallID == ev.CallID && other.SessionID == ev.SessionID && other.ID != ev.ID {
				if len(p.Linked) == 100 {
					return fmt.Errorf("tool-event links exceed 100; evidence metadata cannot be represented safely")
				}
				p.Linked = append(p.Linked, other.ID)
				callSeen = callSeen || other.Category == record.CategoryToolCall
				resultSeen = resultSeen || other.Category == record.CategoryToolResult
			}
			if progress, ok := entry.Rec.(*record.Progress); ok && progress.Stage == "capture_gap" && strings.HasPrefix(progress.Cursor, ev.SessionID+"/") {
				p.CaptureGaps++
			}
			return nil
		}); err != nil {
			return nil, err
		}
		if idx != nil {
			linked, err := idx.Linked(ev)
			if err != nil {
				return nil, err
			}
			if len(linked) != len(p.Linked) {
				return nil, fmt.Errorf("indexed links differ from stored source; rebuild index")
			}
			for i, linkedID := range linked {
				if linkedID != p.Linked[i] {
					return nil, fmt.Errorf("indexed links differ from stored source; rebuild index")
				}
				partner, err := idx.Read(linkedID)
				if err != nil {
					return nil, err
				}
				if partner.SessionID != ev.SessionID || partner.CallID != ev.CallID {
					return nil, fmt.Errorf("indexed partner differs from stored source; rebuild index")
				}
			}
		}
		p.MissingPartner = ev.CallID != "" && (!callSeen || !resultSeen)
		p.Complete = cursor == len(ev.Content)
		pages = append(pages, p)
	}
	encode := func() ([]byte, error) {
		var value any = pages
		if len(pages) == 1 {
			value = pages[0]
		}
		return json.Marshal(value)
	}
	// Seed each unfinished event with one whole code point, so no successful
	// response stalls. All required metadata and JSON escaping count as output.
	for i := range pages {
		p := &pages[i]
		content := events[p.EventID].Content
		if p.Next < len(content) {
			_, n := utf8.DecodeRuneInString(content[p.Next:])
			p.Next += n
			p.Content = content[p.Cursor:p.Next]
			p.Complete = p.Next == len(content)
		}
	}
	out, err := encode()
	if err != nil {
		return nil, err
	}
	if len(out)+1 > budget*4 {
		return nil, fmt.Errorf("budget cannot contain required evidence metadata and one whole code point per event; increase budget")
	}
	// Binary-search a bounded UTF-8 prefix instead of allocating per code point.
	for i := range pages {
		p := &pages[i]
		content := events[p.EventID].Content
		low, high := p.Next, min(len(content), p.Cursor+budget*4)
		for high < len(content) && high > low && !utf8.RuneStart(content[high]) {
			high--
		}
		best := p.Next
		for low <= high {
			end := low + (high-low)/2
			for end > best && end < len(content) && !utf8.RuneStart(content[end]) {
				end--
			}
			p.Next = end
			p.Content = content[p.Cursor:end]
			p.Complete = end == len(content)
			candidate, err := encode()
			if err != nil {
				return nil, err
			}
			if len(candidate)+1 <= budget*4 {
				best = end
				out = candidate
				if end == len(content) {
					break
				}
				_, n := utf8.DecodeRuneInString(content[end:])
				low = end + n
			} else {
				high = end - 1
			}
		}
		p.Next = best
		p.Content = content[p.Cursor:best]
		p.Complete = best == len(content)
	}
	return out, nil
}
