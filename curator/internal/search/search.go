// Package search ranks journaled conversation events against a few query
// words. It is read-only and its output is bounded by hit count and snippet
// length, so a broad query cannot flood an agent's context the way a raw grep
// over whole journal lines can.
package search

import (
	"errors"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"pi-curator/curator/internal/memory"
	"pi-curator/curator/internal/record"
)

// Defaults and ceilings for Options.
const (
	DefaultLimit        = 8
	MaxLimit            = 50
	DefaultSnippetRunes = 400
	// DecisionSnippetRunes is the room a decision hit gets when the caller
	// did not choose a snippet size; a user's stated rule is short and is
	// the text an agent most needs verbatim.
	DecisionSnippetRunes = 1000
	MaxSnippetRunes      = 2000
	minPrefixRunes       = 3
	userBonus            = 0.25
	decisionBonus        = 0.5
	toolWeight           = 0.4 // tool events rank below what people said about the same file
	goalRunes            = 160
)

// Options controls one search. Zero Limit and SnippetRunes select the defaults.
type Options struct {
	Terms         []string
	File          string
	DecisionsOnly bool
	Limit         int
	SnippetRunes  int
}

// Hit is one matching event.
type Hit struct {
	EventID      string   `json:"event_id"`
	CreatedAtUTC string   `json:"created_at_utc"`
	Category     string   `json:"category"`
	Score        float64  `json:"score"`
	Decision     bool     `json:"decision,omitempty"`
	Snippet      string   `json:"snippet"`
	MemoryID     string   `json:"memory_id,omitempty"`
	EvidenceIDs  []string `json:"evidence_event_ids,omitempty"`
}

// Session groups the hits of one earlier session, best hit first.
type Session struct {
	SessionID    string `json:"session_id"`
	CreatedAtUTC string `json:"created_at_utc"`
	Goal         string `json:"goal,omitempty"` // the session's opening request, so a hit can be placed without opening it
	Hits         []Hit  `json:"hits"`
}

// Result is the ranked, bounded answer. Matched counts every matching event
// before the limit, so a caller can tell a narrow answer from a cut one.
type Result struct {
	Terms    []string  `json:"terms"`
	File     string    `json:"file,omitempty"`
	Matched  int       `json:"matched"`
	Sessions []Session `json:"sessions"`
}

type token struct {
	text  string
	start int // rune offset in the content
}

func tokenize(s string) []token {
	var out []token
	var cur []rune
	start := 0
	pos := 0
	flush := func() {
		if len(cur) > 0 {
			out = append(out, token{text: string(cur), start: start})
			cur = cur[:0]
		}
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			if len(cur) == 0 {
				start = pos
			}
			cur = append(cur, unicode.ToLower(r))
		} else {
			flush()
		}
		pos++
	}
	flush()
	return out
}

// queryTerms lowercases and de-duplicates the query words.
func queryTerms(words []string) []string {
	seen := map[string]bool{}
	var terms []string
	for _, w := range words {
		for _, t := range tokenize(w) {
			if !seen[t.text] {
				seen[t.text] = true
				terms = append(terms, t.text)
			}
		}
	}
	return terms
}

// matches reports whether a content token satisfies a term: equality, or a
// prefix for terms long enough that the prefix is not noise ("sort" finds
// "sorted" and "sorting").
func matches(term, tok string) bool {
	if tok == term {
		return true
	}
	return len([]rune(term)) >= minPrefixRunes && strings.HasPrefix(tok, term)
}

// Run ranks the conversation events of d (user and assistant messages). With
// opts.File set it instead finds every event, tool calls and results included,
// that mentions the file, so "which sessions touched config.local.ini" does not
// depend on how anyone described the change; words then only rank the matches.
// With opts.DecisionsOnly only user decisions qualify, newest first when no
// word is given.
func Run(d *memory.Data, opts Options) (Result, error) {
	return run(d, opts, false)
}

func run(d *memory.Data, opts Options, wholeSnippet bool) (Result, error) {
	terms := queryTerms(opts.Terms)
	file := strings.ToLower(strings.TrimSpace(opts.File))
	if len(terms) == 0 && file == "" && !opts.DecisionsOnly {
		return Result{}, errors.New("search needs at least one word, --file or --decisions")
	}
	limit := clamp(opts.Limit, DefaultLimit, MaxLimit)
	snippet := clamp(opts.SnippetRunes, DefaultSnippetRunes, MaxSnippetRunes)

	type scored struct {
		ev    *record.Event
		hit   Hit
		order int
	}
	var all []scored
	for i, id := range d.EventOrder {
		ev := d.Events[id]
		conversation := ev.Category == record.CategoryUserMessage || ev.Category == record.CategoryAssistantMessage
		if !conversation && file == "" {
			continue
		}
		if opts.DecisionsOnly && !(ev.Category == record.CategoryUserMessage && IsDecision(ev.Content)) {
			continue
		}
		sc, at, ok := 1.0, 0, true
		if file != "" {
			lower := strings.ToLower(ev.Content)
			idx := strings.Index(lower, file)
			if idx < 0 {
				continue
			}
			at = utf8.RuneCountInString(lower[:idx])
		}
		if len(terms) > 0 {
			if sc, at, ok = score(ev, terms, at, file != ""); !ok {
				continue
			}
		}
		if !conversation {
			sc *= toolWeight
		}
		decision := ev.Category == record.CategoryUserMessage && IsDecision(ev.Content)
		room := snippet
		if decision && opts.SnippetRunes == 0 {
			room = DecisionSnippetRunes // a stated rule is read whole unless the caller set a size
		}
		text := ev.Content
		if !wholeSnippet {
			text = window(ev.Content, at, room)
		}
		all = append(all, scored{ev: ev, order: i, hit: Hit{
			EventID: ev.ID, CreatedAtUTC: ev.CreatedAtUTC, Category: ev.Category,
			Score: sc, Decision: decision,
			Snippet: text,
		}})
	}
	// Best score first; among equals the newer event, which a later decision
	// would have superseded an older one with.
	sort.SliceStable(all, func(a, b int) bool {
		if all[a].hit.Score != all[b].hit.Score {
			return all[a].hit.Score > all[b].hit.Score
		}
		if all[a].hit.CreatedAtUTC != all[b].hit.CreatedAtUTC {
			return all[a].hit.CreatedAtUTC > all[b].hit.CreatedAtUTC
		}
		return all[a].order > all[b].order
	})
	res := Result{Terms: append([]string{}, terms...), File: file, Matched: len(all), Sessions: []Session{}}
	if len(all) > limit {
		all = all[:limit]
	}
	goals := map[string]string{}
	for _, id := range d.EventOrder {
		ev := d.Events[id]
		if _, seen := goals[ev.SessionID]; !seen && ev.Category == record.CategoryUserMessage {
			goals[ev.SessionID] = window(ev.Content, 0, goalRunes)
		}
	}
	index := map[string]int{}
	for _, s := range all {
		i, seen := index[s.ev.SessionID]
		if !seen {
			i = len(res.Sessions)
			index[s.ev.SessionID] = i
			res.Sessions = append(res.Sessions, Session{SessionID: s.ev.SessionID, CreatedAtUTC: s.ev.CreatedAtUTC, Goal: goals[s.ev.SessionID]})
		}
		sess := &res.Sessions[i]
		sess.Hits = append(sess.Hits, s.hit)
		if s.hit.CreatedAtUTC < sess.CreatedAtUTC {
			sess.CreatedAtUTC = s.hit.CreatedAtUTC
		}
	}
	return res, nil
}

// score returns the share of query terms the event contains (plus a bonus for
// user messages, where rules are stated) and the rune offset of the first match.
// With filtered set the event already matched a file filter at rune offset
// fileAt, so it is kept even when no word matches.
func score(ev *record.Event, terms []string, fileAt int, filtered bool) (float64, int, bool) {
	found := make([]bool, len(terms))
	first := -1
	for _, tok := range tokenize(ev.Content) {
		for i, term := range terms {
			if matches(term, tok.text) {
				found[i] = true
				if first < 0 {
					first = tok.start
				}
			}
		}
	}
	n := 0
	for _, f := range found {
		if f {
			n++
		}
	}
	if n == 0 && !filtered {
		return 0, 0, false
	}
	s := float64(n) / float64(len(terms))
	if ev.Category == record.CategoryUserMessage {
		s += userBonus
		if IsDecision(ev.Content) {
			s += decisionBonus
		}
	}
	if filtered {
		// The file already matched; the words only rank, and the snippet
		// stays on the file mention.
		return s, fileAt, true
	}
	return s, first, true
}

// decisionMarker finds the phrases people use when they settle a rule.
var decisionMarker = regexp.MustCompile(`(?i)\bdecision\s*:|\bagreed\b|\bfrom now on\b|\bgoing forward\b|\bwe decided\b|\bfrom day one\b|\balways\b|\bnever\b`)

// IsDecision reports whether a user message states a decision or rule. It is
// computed when searching, not stored, so it applies to journals captured
// before it existed and its markers can change without rewriting history.
func IsDecision(content string) bool { return decisionMarker.MatchString(content) }

// window cuts at most max runes of content around the rune offset at, with
// whitespace collapsed, and marks each cut end with an ellipsis.
func window(content string, at, max int) string {
	r := []rune(content)
	from := at - max/4
	if from < 0 {
		from = 0
	}
	to := from + max
	if to > len(r) {
		to = len(r)
		if from = to - max; from < 0 {
			from = 0
		}
	}
	out := strings.Join(strings.Fields(string(r[from:to])), " ")
	if from > 0 {
		out = "…" + out
	}
	if to < len(r) {
		out += "…"
	}
	return out
}

func clamp(v, def, max int) int {
	switch {
	case v <= 0:
		return def
	case v > max:
		return max
	}
	return v
}
