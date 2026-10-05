package index

import (
	"encoding/json"
	"fmt"
	"pi-curator/curator/internal/record"
	"pi-curator/curator/internal/search"
	"regexp"
	"sort"
	"strings"
)

type anchor struct{ kind, value string }

var pathPattern = regexp.MustCompile(`(?:[A-Za-z0-9_.-]+/)*[A-Za-z0-9_.-]+\.(?:go|py|tsx?|jsx?|rs|c|h|cpp|java|json|ya?ml|toml|ini|md|txt|csv)\b`)
var symbolPattern = regexp.MustCompile(`\b(?:[A-Z][a-z]+[A-Za-z0-9_]*|[A-Za-z_][A-Za-z0-9_]*\(\))`)
var testPattern = regexp.MustCompile(`\b(?:test_[A-Za-z0-9_]+|Test[A-Za-z0-9_]+)\b`)
var errorPattern = regexp.MustCompile(`\b(?:[A-Za-z0-9_]*(?:Error|Exception)|[A-Z][A-Z0-9_]+)\b`)

func anchors(ev *record.Event) []anchor {
	seen := map[anchor]bool{}
	out := []anchor{}
	for _, pattern := range []struct {
		kind  string
		regex *regexp.Regexp
	}{{"file", pathPattern}, {"symbol", symbolPattern}, {"test", testPattern}, {"error", errorPattern}} {
		for _, value := range pattern.regex.FindAllString(ev.Content, 128) {
			value = strings.TrimSuffix(value, "()")
			a := anchor{pattern.kind, value}
			if !seen[a] {
				seen[a] = true
				out = append(out, a)
			}
		}
	}
	if ev.Category == record.CategoryToolCall && ev.ToolName == "bash" {
		var args struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(ev.Content), &args) == nil && args.Command != "" {
			out = append(out, anchor{"command", args.Command})
		}
	}
	return out
}

// Hybrid unions lexical candidates with exact code anchors. Exact matches add
// a small ranking bonus, not an assertion about root cause or outcome.
func (idx *Index) Hybrid(query string, limit int) (search.Result, error) {
	if len(strings.Fields(query)) > 20 || len(query) > 4096 {
		return search.Result{}, fmt.Errorf("query exceeds 20 terms or 4096 bytes")
	}
	res, err := idx.Search(query, 50)
	if err != nil {
		return res, err
	}
	type candidate struct {
		hit           search.Hit
		session, goal string
	}
	candidates := map[string]candidate{}
	for _, session := range res.Sessions {
		for _, hit := range session.Hits {
			candidates[hit.EventID] = candidate{hit, session.SessionID, session.Goal}
		}
	}
	keys := append([]string{strings.TrimSpace(query)}, strings.Fields(query)...)
	seen := map[string]bool{}
	for _, key := range keys {
		key = strings.Trim(key, "`\"'(),;:")
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		rows, err := idx.db.Query("SELECT DISTINCT event_id FROM anchors WHERE value=? LIMIT 50", key)
		if err != nil {
			return res, err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return res, err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return res, err
		}
		for _, id := range ids {
			c, exists := candidates[id]
			if !exists {
				if len(candidates) >= 100 {
					continue
				}
				ev, err := idx.Read(id)
				if err != nil {
					return res, err
				}
				text := []rune(ev.Content)
				if len(text) > 400 {
					text = append(text[:400], '…')
				}
				c = candidate{hit: search.Hit{EventID: id, CreatedAtUTC: ev.CreatedAtUTC, Category: ev.Category, Snippet: string(text), Score: 0.4}, session: ev.SessionID}
			}
			c.hit.Score += 0.3
			candidates[id] = c
		}
	}
	ordered := make([]candidate, 0, len(candidates))
	for _, c := range candidates {
		ordered = append(ordered, c)
	}
	sort.Slice(ordered, func(a, b int) bool {
		if ordered[a].hit.Score != ordered[b].hit.Score {
			return ordered[a].hit.Score > ordered[b].hit.Score
		}
		if ordered[a].hit.CreatedAtUTC != ordered[b].hit.CreatedAtUTC {
			return ordered[a].hit.CreatedAtUTC > ordered[b].hit.CreatedAtUTC
		}
		return ordered[a].hit.EventID < ordered[b].hit.EventID
	})
	result := search.Result{Terms: res.Terms, Matched: len(ordered), Sessions: []search.Session{}}
	positions := map[string]int{}
	for _, c := range ordered[:min(limit, len(ordered))] {
		n, ok := positions[c.session]
		if !ok {
			n = len(result.Sessions)
			positions[c.session] = n
			result.Sessions = append(result.Sessions, search.Session{SessionID: c.session, CreatedAtUTC: c.hit.CreatedAtUTC, Goal: c.goal})
		}
		result.Sessions[n].Hits = append(result.Sessions[n].Hits, c.hit)
	}
	return result, nil
}
