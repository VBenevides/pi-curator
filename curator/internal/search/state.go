package search

import (
	"fmt"
	"pi-curator/curator/internal/memory"
	"pi-curator/curator/internal/record"
)

// State searches only explicitly declared matching namespaces. Multiple active
// declarations remain candidates; ordering does not replace contradictory facts.
func State(d *memory.Data, opts Options, scope record.Scope, history bool) (Result, error) {
	cards := &memory.Data{Events: make(map[string]*record.Event)}
	facts := make(map[string]*record.Memory)
	lifecycle := record.Replay(d.Lifecycle)
	for _, fact := range d.States {
		if fact.Scope.Repository != scope.Repository || (fact.Scope.Branch != "" && fact.Scope.Branch != scope.Branch) {
			continue
		}
		status := lifecycle[fact.ID]
		if !history && (status.Archived || status.SupersededBy != "") {
			continue
		}
		text := fmt.Sprintf("Declared state %s; repository %s; branch %q. Multiple declarations may conflict; timestamps do not replace facts. Archived=%t superseded_by=%q. [%s]: %s", fact.StateKey, fact.Scope.Repository, fact.Scope.Branch, status.Archived, status.SupersededBy, fact.SourceRefs[0], fact.Summary)
		cards.Events[fact.ID] = &record.Event{ID: fact.ID, SessionID: fact.Session, CreatedAtUTC: fact.CreatedAtUTC, Category: record.CategoryUserMessage, Content: text}
		cards.EventOrder = append(cards.EventOrder, fact.ID)
		facts[fact.ID] = fact
	}
	result, err := run(cards, opts, true)
	if err != nil {
		return result, err
	}
	for si := range result.Sessions {
		for hi := range result.Sessions[si].Hits {
			hit := &result.Sessions[si].Hits[hi]
			fact := facts[hit.EventID]
			_, id, err := record.ParseRef(fact.SourceRefs[0])
			if err != nil {
				return Result{}, err
			}
			hit.MemoryID = fact.ID
			hit.EventID = id
			hit.EvidenceIDs = []string{id}
			hit.Category = "declared_state"
		}
	}
	return result, nil
}
