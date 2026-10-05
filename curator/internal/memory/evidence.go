package memory

import (
	"fmt"
	"slices"

	"pi-curator/curator/internal/record"
)

// addEvidence records attribution and host-observed tool status. Text such as
// "PASS" and an assistant's claim never establishes successful execution or
// task completion. The boundary closes an episode, not an outcome.
func addEvidence(ep *record.Episode, d *Data, m *record.Memory) {
	calls := make(map[string]*record.Event)
	type inputKey struct{ tool, input string }
	failures := make(map[inputKey]record.Attempt)
	for _, id := range ep.EventIDs {
		ev := d.Events[id]
		if ev == nil || ev.SessionID != ep.SessionID {
			m.EvidenceGaps = append(m.EvidenceGaps, fmt.Sprintf("missing or wrong-session source: %s", id))
			continue
		}
		switch ev.Category {
		case record.CategoryUserMessage, record.CategoryAssistantMessage:
			if text := oneLine(ev.Content, MaxFieldRunes); text != "" {
				kind := "user_statement"
				if ev.Category == record.CategoryAssistantMessage {
					kind = "assistant_statement"
				}
				m.Claims = append(m.Claims, record.Claim{Kind: kind, Text: text, SourceRefs: []string{record.EventRef(id)}})
			}
		case record.CategoryToolCall:
			if ev.CallID != "" {
				if _, exists := calls[ev.CallID]; exists {
					calls[ev.CallID] = nil
					m.EvidenceGaps = append(m.EvidenceGaps, fmt.Sprintf("ambiguous tool call: %s", ev.CallID))
				} else {
					calls[ev.CallID] = ev
				}
			}
		case record.CategoryToolResult:
			attempt := record.Attempt{Tool: ev.ToolName, Observation: "unknown"}
			if ev.IsError != nil {
				attempt.Observation = "tool_succeeded"
				if *ev.IsError {
					attempt.Observation = "tool_failed"
				}
			}
			call := calls[ev.CallID]
			if call != nil && call.ToolName == ev.ToolName {
				attempt.Input = oneLine(call.Content, MaxFieldRunes)
				attempt.SourceRefs = []string{record.EventRef(call.ID), record.EventRef(id)}
			} else {
				attempt.SourceRefs = []string{record.EventRef(id)}
				if ev.CallID != "" {
					m.EvidenceGaps = append(m.EvidenceGaps, fmt.Sprintf("tool result %s has no matching call in this episode", id))
				}
				call = nil
			}
			m.Attempts = append(m.Attempts, attempt)
			if text := oneLine(ev.Content, MaxFieldRunes); text != "" {
				m.Claims = append(m.Claims, record.Claim{Kind: "tool_output", Text: text, SourceRefs: []string{record.EventRef(id)}})
			}
			if call == nil {
				continue
			}
			key := inputKey{call.ToolName, call.Content}
			if attempt.Observation == "tool_failed" {
				failures[key] = attempt
			}
			if previous, ok := failures[key]; ok && attempt.Observation == "tool_succeeded" {
				refs := slices.Clone(previous.SourceRefs)
				for _, ref := range attempt.SourceRefs {
					if !slices.Contains(refs, ref) {
						refs = append(refs, ref)
					}
				}
				m.Lessons = append(m.Lessons, record.Claim{Kind: "execution_transition", Text: "The same tool input failed before it succeeded; this observation does not establish a causal fix.", SourceRefs: refs})
				delete(failures, key)
			}
		}
	}
}
