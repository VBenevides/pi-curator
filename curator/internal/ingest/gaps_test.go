package ingest

import (
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"

	"pi-curator/curator/internal/journal"
	"pi-curator/curator/internal/record"
)

func gapOutcomes(r Response) string { return outcomes(Response{Results: r.Gaps}) }

func TestReportedGapStaysExplicitUntilEventIsRecoveredIdempotently(t *testing.T) {
	root := readyRepo(t)
	store := journal.New(filepath.Join(root, ".curator"))
	req := Request{Gaps: []GapIn{{SessionID: "s1", EventID: "lost1"}, {SessionID: "s1", EventID: "lost2"}, {SessionID: "bad session", EventID: "x"}}}
	r := Run(root, req, opts())
	if r.Gaps[0].Outcome != OutcomeDurable || r.Gaps[1].Outcome != OutcomeDurable || r.Gaps[2].Outcome != OutcomeRejected {
		t.Fatalf("gaps: %+v", r.Gaps)
	}
	if again := Run(root, req, opts()); again.Gaps[0].Outcome != OutcomeDuplicate || again.Gaps[1].Outcome != OutcomeDuplicate {
		t.Fatalf("repeat: %+v", again.Gaps)
	}
	got, err := Gaps(store)
	if err != nil || len(got) != 2 || got[0].Recovered || got[1].Recovered {
		t.Fatalf("gaps = %+v, %v", got, err)
	}
	// Recovering lost1 from host history closes only that gap; replaying twice is safe.
	rec := Request{Events: []EventIn{ev("lost1", record.CategoryUserMessage, "recovered")}}
	Run(root, rec, opts())
	if again := Run(root, rec, opts()); again.Results[0].Outcome != OutcomeDuplicate {
		t.Fatalf("replay: %+v", again.Results)
	}
	got, _ = Gaps(store)
	if !got[0].Recovered || got[1].Recovered {
		t.Fatalf("after recovery: %+v", got)
	}
}

func TestSimultaneousIngestersLoseNothingAndDuplicateNothing(t *testing.T) {
	root := readyRepo(t)
	o := opts()
	o.LockTimeout = 5e9
	var wg sync.WaitGroup
	for s := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			session := string(rune('a' + s))
			for i := range 5 {
				id := session + string(rune('0'+i))
				req := Request{Events: []EventIn{{ID: id, SessionID: session, Category: record.CategoryUserMessage, Content: id}}}
				for range 2 { // repeated callback
					if r := Run(root, req, o); r.Results[0].Outcome == OutcomeFailed {
						t.Errorf("%s failed: %+v", id, r.Results[0])
					}
				}
			}
		}()
	}
	wg.Wait()
	if ids := journalIDs(t, root); len(ids) != 20 {
		t.Fatalf("journal holds %d events, want 20", len(ids))
	}
}

func TestOversizedBatchIsRefusedWholesale(t *testing.T) {
	var req Request
	for i := range MaxBatchEvents + 1 {
		req.Events = append(req.Events, ev("e"+string(rune('a'+i%26))+string(rune('a'+i/26)), record.CategoryUserMessage, "x"))
	}
	data, _ := json.Marshal(req)
	if _, err := ParseRequest(data); err == nil {
		t.Fatal("oversized batch accepted")
	}
}
