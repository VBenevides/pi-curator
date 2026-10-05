package search

import (
	"regexp"
	"slices"
	"testing"

	"pi-curator/curator/internal/memory"
	"pi-curator/curator/internal/record"
)

func TestEpisodeCardsExposeResolvableSourcesNotTaskSuccess(t *testing.T) {
	const stamp = "2026-10-03T00:00:00Z"
	d := &memory.Data{Events: map[string]*record.Event{
		"intro": {ID: "intro", SessionID: "s", Category: record.CategoryUserMessage, Content: "Implement a roster sorter", CreatedAtUTC: stamp},
		"rule":  {ID: "rule", SessionID: "s", Category: record.CategoryUserMessage, Content: "Preserve accents and original names while sorting", CreatedAtUTC: stamp},
		"claim": {ID: "claim", SessionID: "s", Category: record.CategoryAssistantMessage, Content: "All tests passed", CreatedAtUTC: stamp},
	}, EventOrder: []string{"intro", "rule", "claim"}, Episodes: []*record.Episode{{ID: "ep_original", SessionID: "s", CreatedAtUTC: stamp, EventIDs: []string{"intro", "rule", "claim"}, ExtractionVersion: "extract.v1"}}}
	res, err := Episodes(d, Options{Terms: []string{"preserve"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Matched != 1 || len(res.Sessions) != 1 {
		t.Fatalf("missing episode: %+v", res)
	}
	hit := res.Sessions[0].Hits[0]
	if hit.Category != "derived_episode" || !slices.Contains(hit.EvidenceIDs, "rule") || d.Events[hit.EventID] == nil {
		t.Fatalf("unreadable candidate: %+v", hit)
	}
	refs := regexp.MustCompile(`(?:event|episode):[A-Za-z0-9._:@/-]+`).FindAllString(hit.Snippet, -1)
	for _, ref := range refs {
		kind, id, err := record.ParseRef(ref)
		if err != nil {
			t.Fatal(err)
		}
		if kind == record.RefEpisode && id != d.Episodes[0].ID || kind == record.RefEvent && d.Events[id] == nil {
			t.Fatalf("unresolvable displayed provenance: %s", ref)
		}
	}
	if len(refs) == 0 {
		t.Fatal("card omitted source provenance")
	}
}
