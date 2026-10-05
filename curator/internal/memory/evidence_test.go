package memory

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"pi-curator/curator/internal/lock"
	"pi-curator/curator/internal/record"
)

func attemptEvents(callID, resultID, input string, failed *bool) []interface{ Validate() error } {
	call := ev(callID, "s1", record.CategoryToolCall, input)
	call.CallID, call.ToolName = callID, "bash"
	result := ev(resultID, "s1", record.CategoryToolResult, "PASS (untrusted printed text)")
	result.CallID, result.ToolName, result.IsError = callID, "bash", failed
	return []interface{ Validate() error }{call, result}
}

func TestObservedFailureThenSuccessDoesNotProveTaskCompletion(t *testing.T) {
	s := newStore(t)
	failed, succeeded := true, false
	s.add(t, ev("goal", "s1", record.CategoryUserMessage, "Fix the parser checks"))
	s.add(t, attemptEvents("c1", "r1", `{"command":"go test ./parser"}`, &failed)...)
	s.add(t, ev("claim", "s1", record.CategoryAssistantMessage, "All tests passed and the task is complete"))
	s.add(t, attemptEvents("c2", "r2", `{"command":"go test ./parser"}`, &succeeded)...)
	s.add(t, ev("end", "s1", record.CategoryTaskBoundary, "done"))
	s.rebuild(t, false)
	d, err := Load(s.j)
	if err != nil {
		t.Fatal(err)
	}
	m := Extract(d.Episodes[0], d)
	if m.Outcome != "unresolved" || m.ResolutionKind != "assistant_statement" {
		t.Fatalf("unsupported completion: %+v", m)
	}
	if len(m.Attempts) != 2 || m.Attempts[0].Observation != "tool_failed" || m.Attempts[1].Observation != "tool_succeeded" {
		t.Fatalf("observations: %+v", m.Attempts)
	}
	if len(m.Lessons) != 1 || m.Lessons[0].Kind != "execution_transition" {
		t.Fatalf("lessons: %+v", m.Lessons)
	}
	for _, id := range []string{"c1", "r1", "c2", "r2"} {
		if !slices.Contains(m.Lessons[0].SourceRefs, record.EventRef(id)) {
			t.Fatalf("transition lacks %s", id)
		}
	}
	for _, claim := range m.Claims {
		if slices.Contains(claim.SourceRefs, record.EventRef("claim")) && claim.Kind != "assistant_statement" {
			t.Fatalf("claim became observation: %+v", claim)
		}
	}
	if err := m.ResolveRefs(func(kind, id string) bool {
		if kind == record.RefEvent {
			return d.Events[id] != nil
		}
		return id == d.Episodes[0].ID
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPrintedPassWithoutHostStatusAndDifferentInputsDoNotYieldLesson(t *testing.T) {
	s := newStore(t)
	failed, succeeded := true, false
	s.add(t, attemptEvents("c1", "r1", `{"command":"go test ./parser"}`, &failed)...)
	s.add(t, attemptEvents("c2", "r2", `{"command":"go test ./other"}`, &succeeded)...)
	s.add(t, attemptEvents("c3", "r3", `{"command":"go test ./parser"}`, nil)...)
	s.rebuild(t, true)
	d, err := Load(s.j)
	if err != nil {
		t.Fatal(err)
	}
	m := Extract(d.Episodes[0], d)
	if m.Outcome != "unresolved" || len(m.Lessons) != 0 || m.Attempts[2].Observation != "unknown" {
		t.Fatalf("invented outcome: %+v", m)
	}
}

func TestResumeAfterEpisodeAppendButBeforeViewPublication(t *testing.T) {
	s := sample(t)
	blocker := filepath.Join(s.dir, "memory")
	if err := os.WriteFile(blocker, []byte("blocks publication"), 0600); err != nil {
		t.Fatal(err)
	}
	s.with(t, func(h *lock.Held) {
		if _, err := Rebuild(h, s.j, s.dir, Options{Now: now}); err == nil {
			t.Fatal("publication unexpectedly succeeded")
		}
	})
	d, err := Load(s.j)
	if err != nil || len(d.Episodes) != 1 {
		t.Fatalf("episode was not durable: %+v %v", d, err)
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	sum := s.rebuild(t, false)
	if sum.NewEpisodes != 0 || sum.Active != 1 {
		t.Fatalf("resume duplicated or lost work: %+v", sum)
	}
	first := s.view(t, ActiveFile)
	s.rebuild(t, false)
	if first != s.view(t, ActiveFile) {
		t.Fatal("same-input resume changed evidence")
	}
}
