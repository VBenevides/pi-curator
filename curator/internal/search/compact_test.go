package search

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCompactBoundsWholeResponseAndReportsOmissions(t *testing.T) {
	for _, budget := range []int{64, 100, 250} {
		res := Result{Matched: 10, Sessions: []Session{{Goal: strings.Repeat("雪", 100), Hits: []Hit{{EventID: strings.Repeat("long", 1000), Snippet: "must be omitted"}, {EventID: "e2", CreatedAtUTC: "2026-10-03", Category: "user_message", Snippet: strings.Repeat("雪", 1000)}}}}}
		text := Compact(res, budget)
		if len(text) > budget*4 || !utf8.ValidString(text) {
			t.Fatalf("budget %d: invalid or too large %d", budget, len(text))
		}
		if strings.Contains(text, "must be omitted") || !strings.Contains(text, "Omitted matches:") {
			t.Fatalf("missing omission: %s", text)
		}
	}
	if text := Compact(Result{}, 64); !strings.Contains(text, "Omitted matches: 0") {
		t.Fatal(text)
	}
}
