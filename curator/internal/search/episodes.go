package search

import (
	"fmt"
	"strings"

	"pi-curator/curator/internal/memory"
	"pi-curator/curator/internal/record"
)

// Episodes ranks independently derived episode cards with the existing lexical
// scorer. It does not replace raw-event retrieval. Returned IDs still name exact
// journal events; diagnostic evidence IDs describe the candidate's source set.
func Episodes(d *memory.Data, opts Options) (Result, error) {
	cards := &memory.Data{Events: make(map[string]*record.Event)}
	derived := make(map[string]*record.Memory)
	states := record.Replay(d.Lifecycle)
	for _, ep := range d.Episodes {
		m := memory.Extract(ep, d)
		if m == nil {
			continue
		}
		state := states[m.ID]
		if state.Archived || state.SupersededBy != "" {
			continue
		}
		source := record.Event{ID: ep.ID, SessionID: ep.SessionID, CreatedAtUTC: ep.CreatedAtUTC, Category: record.CategoryAssistantMessage, Content: episodeCard(m)}
		cards.Events[ep.ID] = &source
		cards.EventOrder = append(cards.EventOrder, ep.ID)
		derived[ep.ID] = m
	}
	res, err := run(cards, opts, true)
	if err != nil {
		return res, err
	}
	for si := range res.Sessions {
		session := &res.Sessions[si]
		for hi := range session.Hits {
			hit := &session.Hits[hi]
			m := derived[hit.EventID]
			hit.MemoryID = m.ID
			hit.Category = "derived_episode"
			for _, ref := range m.SourceRefs {
				kind, id, err := record.ParseRef(ref)
				if err != nil {
					return Result{}, err
				}
				if kind == record.RefEvent {
					hit.EvidenceIDs = append(hit.EvidenceIDs, id)
				}
			}
			if len(hit.EvidenceIDs) == 0 {
				return Result{}, fmt.Errorf("episode %s has no available source events", hit.MemoryID)
			}
			hit.EventID = hit.EvidenceIDs[0]
			if hi == 0 {
				session.Goal = m.Goal
			}
		}
	}
	return res, nil
}

// Keep labels and references before excerpts so even a compact prefix cannot
// turn an attributed statement into an observed successful task outcome.
func episodeCard(m *record.Memory) string {
	const maxBytes = 8000
	const footer = "\nAdditional episode evidence omitted; use exact source reads."
	var out strings.Builder
	out.WriteString("Task outcome: ")
	out.WriteString(m.Outcome)
	out.WriteString(". ")
	out.WriteString(m.SourceRefs[0])
	add := func(kind string, refs []string, text string) bool {
		size := len(kind) + len(text) + 7
		for _, ref := range refs {
			size += len(ref) + 1
		}
		if out.Len()+size > maxBytes-len(footer) {
			return false
		}
		out.WriteString("\n")
		out.WriteString(kind)
		out.WriteString(" [")
		for i, ref := range refs {
			if i > 0 {
				out.WriteString(",")
			}
			out.WriteString(ref)
		}
		out.WriteString("]: ")
		out.WriteString(text)
		return true
	}
	for _, claim := range m.Claims {
		if claim.Kind == "user_statement" && !add(claim.Kind, claim.SourceRefs, claim.Text) {
			out.WriteString(footer)
			return out.String()
		}
	}
	for _, attempt := range m.Attempts {
		if attempt.Observation != "unknown" && !add(attempt.Observation, attempt.SourceRefs, attempt.Input) {
			out.WriteString(footer)
			return out.String()
		}
	}
	for _, lesson := range m.Lessons {
		if !add(lesson.Kind, lesson.SourceRefs, lesson.Text) {
			out.WriteString(footer)
			return out.String()
		}
	}
	for _, claim := range m.Claims {
		if claim.Kind != "user_statement" && !add(claim.Kind, claim.SourceRefs, claim.Text) {
			out.WriteString(footer)
			return out.String()
		}
	}
	for _, gap := range m.EvidenceGaps {
		if !add("evidence_gap", nil, gap) {
			out.WriteString(footer)
			return out.String()
		}
	}
	return out.String()
}
