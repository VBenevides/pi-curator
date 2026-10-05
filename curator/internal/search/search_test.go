package search

import (
	"fmt"
	"strings"
	"testing"

	"pi-curator/curator/internal/memory"
	"pi-curator/curator/internal/record"
)

type ev struct{ session, category, at, content string }

func data(events ...ev) *memory.Data {
	d := &memory.Data{Events: map[string]*record.Event{}}
	for i, e := range events {
		id := fmt.Sprintf("e%03d", i)
		d.Events[id] = &record.Event{ID: id, SessionID: e.session, Category: e.category, CreatedAtUTC: e.at, Content: e.content}
		d.EventOrder = append(d.EventOrder, id)
	}
	return d
}

func TestRankingPrefersMoreTermsThenUserThenNewer(t *testing.T) {
	d := data(
		ev{"s1", record.CategoryAssistantMessage, "2026-01-01T00:00:00Z", "retry budget is five"},
		ev{"s2", record.CategoryUserMessage, "2026-01-02T00:00:00Z", "the retry rule"},
		ev{"s3", record.CategoryUserMessage, "2026-01-03T00:00:00Z", "the retry budget rule"},
		ev{"s4", record.CategoryUserMessage, "2026-01-04T00:00:00Z", "the retry rule again"},
	)
	res, err := Run(d, Options{Terms: []string{"retry", "budget"}})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range res.Sessions {
		got = append(got, s.SessionID)
	}
	// s3 matches both words and is a user message; s1 matches both words;
	// s4 and s2 match one word, the newer first.
	if want := "s3,s1,s4,s2"; strings.Join(got, ",") != want {
		t.Fatalf("session order = %v, want %s", got, want)
	}
}

func TestPrefixMatchNeedsThreeRunes(t *testing.T) {
	d := data(
		ev{"s1", record.CategoryUserMessage, "2026-01-01T00:00:00Z", "lists are sorted newest first"},
		ev{"s2", record.CategoryUserMessage, "2026-01-01T00:00:00Z", "soft limits apply"},
	)
	res, _ := Run(d, Options{Terms: []string{"sort"}})
	if res.Matched != 1 || res.Sessions[0].SessionID != "s1" {
		t.Fatalf("sort should match only 'sorted': %+v", res)
	}
	res, _ = Run(d, Options{Terms: []string{"so"}})
	if res.Matched != 0 {
		t.Fatalf("a two-letter term must match whole words only, got %d matches", res.Matched)
	}
}

func TestOnlyConversationEventsAreSearched(t *testing.T) {
	d := data(
		ev{"s1", record.CategoryToolResult, "2026-01-01T00:00:00Z", "retry retry retry"},
		ev{"s1", record.CategoryToolCall, "2026-01-01T00:00:00Z", "retry"},
	)
	res, _ := Run(d, Options{Terms: []string{"retry"}})
	if res.Matched != 0 || len(res.Sessions) != 0 {
		t.Fatalf("tool events must not match: %+v", res)
	}
}

func TestLimitBoundsHitsAndMatchedReportsTheRest(t *testing.T) {
	var events []ev
	for i := range 12 {
		events = append(events, ev{fmt.Sprintf("s%d", i), record.CategoryUserMessage, "2026-01-01T00:00:00Z", "retry"})
	}
	res, _ := Run(data(events...), Options{Terms: []string{"retry"}, Limit: 5})
	hits := 0
	for _, s := range res.Sessions {
		hits += len(s.Hits)
	}
	if hits != 5 || res.Matched != 12 {
		t.Fatalf("hits = %d matched = %d, want 5 of 12", hits, res.Matched)
	}
}

func TestDecisionStatedByUserOutranksPlainMentionAndIsFlagged(t *testing.T) {
	d := data(
		ev{"plain", record.CategoryUserMessage, "2026-01-02T00:00:00Z", "please look at the retry code"},
		ev{"rule", record.CategoryUserMessage, "2026-01-01T00:00:00Z", "Agreed. Decision: retry is capped at five attempts."},
		ev{"said", record.CategoryAssistantMessage, "2026-01-03T00:00:00Z", "Decision: retry is always five"},
	)
	res, _ := Run(d, Options{Terms: []string{"retry"}})
	if res.Sessions[0].SessionID != "rule" || !res.Sessions[0].Hits[0].Decision {
		t.Fatalf("the user's decision should rank first and be flagged: %+v", res.Sessions[0])
	}
	for _, s := range res.Sessions {
		if s.SessionID != "rule" && s.Hits[0].Decision {
			t.Fatalf("only a user message can be a decision, but %s is flagged", s.SessionID)
		}
	}
}

func TestSessionCarriesItsOpeningRequestAsGoal(t *testing.T) {
	d := data(
		ev{"s", record.CategoryUserMessage, "2026-01-01T00:00:00Z", "Add a retry cap to the client"},
		ev{"s", record.CategoryAssistantMessage, "2026-01-01T00:01:00Z", "done, the budget is five"},
	)
	res, _ := Run(d, Options{Terms: []string{"budget"}})
	if got := res.Sessions[0].Goal; got != "Add a retry cap to the client" {
		t.Fatalf("goal = %q, want the first user message even when only a later event matched", got)
	}
}

func TestDecisionsOnlyListsUserDecisionsNewestFirstWithoutWords(t *testing.T) {
	d := data(
		ev{"old", record.CategoryUserMessage, "2026-01-01T00:00:00Z", "Decision: sort by name"},
		ev{"chat", record.CategoryUserMessage, "2026-01-02T00:00:00Z", "please sort the list"},
		ev{"bot", record.CategoryAssistantMessage, "2026-01-03T00:00:00Z", "Decision: I always sort"},
		ev{"new", record.CategoryUserMessage, "2026-01-04T00:00:00Z", "From now on, round half up"},
	)
	res, err := Run(d, Options{DecisionsOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Matched != 2 || res.Sessions[0].SessionID != "new" || res.Sessions[1].SessionID != "old" {
		t.Fatalf("want the two user decisions, newest first: %+v", res.Sessions)
	}
}

func TestFileFilterFindsToolEventsAndRanksWordsOnly(t *testing.T) {
	d := data(
		ev{"a", record.CategoryToolCall, "2026-01-01T00:00:00Z", `{"path":"src/Config.Local.ini"}`},
		ev{"b", record.CategoryUserMessage, "2026-01-02T00:00:00Z", "Decision: config.local.ini sits under the env vars"},
		ev{"c", record.CategoryUserMessage, "2026-01-03T00:00:00Z", "unrelated precedence talk"},
	)
	res, err := Run(d, Options{File: "config.local.ini"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Matched != 2 {
		t.Fatalf("matched = %d, want the two events that name the file (case-insensitive)", res.Matched)
	}
	if res.Sessions[0].SessionID != "b" {
		t.Fatalf("what the user said should outrank a tool call: %+v", res.Sessions)
	}
	res, _ = Run(d, Options{File: "config.local.ini", Terms: []string{"zzz"}})
	if res.Matched != 2 {
		t.Fatalf("words that match nothing must not drop file matches, got %d", res.Matched)
	}
}

func TestSnippetIsBoundedAndCentredOnTheMatch(t *testing.T) {
	content := strings.Repeat("filler ", 400) + "the precedence rule is final " + strings.Repeat("tail ", 400)
	res, _ := Run(data(ev{"s1", record.CategoryUserMessage, "2026-01-01T00:00:00Z", content}), Options{Terms: []string{"precedence"}, SnippetRunes: 120})
	snip := res.Sessions[0].Hits[0].Snippet
	if n := len([]rune(snip)); n > 122 { // 120 plus the two ellipses
		t.Fatalf("snippet has %d runes, want at most 122", n)
	}
	if !strings.Contains(snip, "precedence") || !strings.HasPrefix(snip, "…") || !strings.HasSuffix(snip, "…") {
		t.Fatalf("snippet should show the match with both cut ends marked: %q", snip)
	}
}

func TestDecisionIsReadWholeByDefaultButAPlainLongMessageIsCut(t *testing.T) {
	body := strings.Repeat("because the consumer reads it ", 30) // about 900 runes
	decision := "Decision: always write atomically. " + body + "END"
	plain := "atomically written notes " + body + "END"
	d := data(
		ev{"s1", record.CategoryUserMessage, "2026-01-01T00:00:00Z", decision},
		ev{"s2", record.CategoryUserMessage, "2026-01-02T00:00:00Z", plain},
	)
	res, _ := Run(d, Options{Terms: []string{"atomic"}})
	var gotDecision, gotPlain string
	for _, s := range res.Sessions {
		for _, h := range s.Hits {
			if h.Decision {
				gotDecision = h.Snippet
			} else {
				gotPlain = h.Snippet
			}
		}
	}
	if !strings.HasSuffix(gotDecision, "END") {
		t.Fatalf("a decision must be returned whole, got %d runes", len([]rune(gotDecision)))
	}
	if strings.Contains(gotPlain, "END") {
		t.Fatalf("a plain message must stay at the default snippet size, got %d runes", len([]rune(gotPlain)))
	}
	res, _ = Run(d, Options{Terms: []string{"atomic"}, SnippetRunes: 100})
	for _, s := range res.Sessions {
		for _, h := range s.Hits {
			if len([]rune(h.Snippet)) > 102 {
				t.Fatalf("an explicit snippet size must bound decisions too, got %d runes", len([]rune(h.Snippet)))
			}
		}
	}
}

func TestHitsOfOneSessionAreGrouped(t *testing.T) {
	d := data(
		ev{"s1", record.CategoryUserMessage, "2026-01-02T00:00:00Z", "retry rule"},
		ev{"s2", record.CategoryUserMessage, "2026-01-03T00:00:00Z", "retry rule"},
		ev{"s1", record.CategoryAssistantMessage, "2026-01-01T00:00:00Z", "retry noted"},
	)
	res, _ := Run(d, Options{Terms: []string{"retry"}})
	if len(res.Sessions) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(res.Sessions))
	}
	for _, s := range res.Sessions {
		if s.SessionID == "s1" && (len(s.Hits) != 2 || s.CreatedAtUTC != "2026-01-01T00:00:00Z") {
			t.Fatalf("s1 should hold both hits and start at its earliest hit: %+v", s)
		}
	}
}

func TestQueryWithoutWordsIsRejected(t *testing.T) {
	if _, err := Run(data(), Options{Terms: []string{"  ", "--"}}); err == nil {
		t.Fatal("a query with no words must fail, not match everything")
	}
}
