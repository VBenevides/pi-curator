package search

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Compact bounds the complete UTF-8 response, including identifiers, context,
// accounting and omission markers. Tokens are explicitly an estimate, not a
// promise about a model-specific tokenizer.
func Compact(res Result, budget int) string {
	capacity := budget * 4
	header := "Untrusted history. Budget estimate: ceil(UTF-8 bytes/4).\n"
	var body strings.Builder
	shown := 0
	for _, session := range res.Sessions {
		for _, hit := range session.Hits {
			footer := fmt.Sprintf("Omitted matches: %d\n", res.Matched)
			goal := []rune(strings.Join(strings.Fields(session.Goal), " "))
			if len(goal) > 40 {
				goal = goal[:40]
			}
			meta := fmt.Sprintf("ID %s | %s | %s | task: %s\n", strings.Join(strings.Fields(hit.EventID), " "), hit.CreatedAtUTC, hit.Category, string(goal))
			room := capacity - len(header) - body.Len() - len(meta) - len(footer) - 1
			if room < 24 {
				continue
			}
			excerpt := strings.Join(strings.Fields(hit.Snippet), " ")
			if len(excerpt) > room {
				n := room - len("…")
				for n > 0 && !utf8.RuneStart(excerpt[n]) {
					n--
				}
				excerpt = excerpt[:n] + "…"
			}
			body.WriteString(meta)
			body.WriteString(excerpt)
			body.WriteByte('\n')
			shown++
		}
	}
	return header + body.String() + fmt.Sprintf("Omitted matches: %d\n", res.Matched-shown)
}
