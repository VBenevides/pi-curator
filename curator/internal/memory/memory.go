// Package memory builds bounded episodes from the authoritative journal,
// derives extractive memories from them, and publishes the agent-readable
// views under .curator/memory/. Every function that mutates state takes the
// *lock.Held proving the caller owns the exclusive memory lock.
//
// Extraction is deterministic: it copies and truncates text that exists in
// events and never invents content, so a rebuild from the same journal gives
// byte-identical views with original creation times preserved.
package memory

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"pi-curator/curator/internal/journal"
	"pi-curator/curator/internal/lock"
	"pi-curator/curator/internal/record"
)

// Bounds on episode and memory size. They keep both construction and the
// grep-readable view bounded no matter how large a session grows.
const (
	MaxEpisodeEvents = 50
	MaxEpisodeBytes  = 64 << 10
	MaxFieldRunes    = 400
	MaxListItems     = 20
	MaxSearchRunes   = 2000

	ActiveFile     = "active.jsonl"
	SupersededFile = "superseded.jsonl"
)

// Status values of Memory.Status.
const (
	StatusActive     = "active"
	StatusSuperseded = "superseded"
)

// Retention values of Memory.Retention in this version (no model scoring yet).
const (
	RetentionDefault = "default"
	RetentionPinned  = "pinned"
)

// Options controls one rebuild.
type Options struct {
	Now func() time.Time
	// Flush also closes each session's trailing open group of events (use at
	// session end or maintenance); otherwise it keeps accumulating.
	Flush bool
}

// Summary reports what a rebuild did.
type Summary struct {
	NewEpisodes int `json:"new_episodes"`
	Episodes    int `json:"episodes"`
	Active      int `json:"active"`
	Superseded  int `json:"superseded"`
	Archived    int `json:"archived"`
	OpenEvents  int `json:"open_events"`
}

// Data is the replayed authoritative state.
type Data struct {
	Events     map[string]*record.Event
	EventOrder []string
	Episodes   []*record.Episode
	Lifecycle  []record.Lifecycle
	States     []*record.Memory
}

// Load reads and validates the whole journal. It refuses a damaged journal
// rather than publishing views derived from a partial reading.
func Load(j *journal.Journal) (*Data, error) {
	d := &Data{Events: map[string]*record.Event{}}
	seenEpisode := map[string]bool{}
	rep, err := j.Scan(func(e journal.Entry) error {
		switch r := e.Rec.(type) {
		case *record.Event:
			if _, dup := d.Events[r.ID]; !dup {
				d.Events[r.ID] = r
				d.EventOrder = append(d.EventOrder, r.ID)
			}
		case *record.Episode:
			if !seenEpisode[r.ID] {
				seenEpisode[r.ID] = true
				d.Episodes = append(d.Episodes, r)
			}
		case *record.Lifecycle:
			d.Lifecycle = append(d.Lifecycle, *r)
		case *record.Memory:
			if r.Kind == "state" {
				d.States = append(d.States, r)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read journal: %w", err)
	}
	if !rep.Healthy() {
		return nil, fmt.Errorf("journal is damaged (%d malformed records, incomplete tail: %t); inspect and recover before rebuilding",
			len(rep.Malformed), rep.Tail != nil)
	}
	seenState := make(map[string]bool)
	validStates := d.States[:0]
	for _, stored := range d.States {
		if seenState[stored.ID] {
			continue
		}
		_, id, refErr := record.ParseRef(stored.SourceRefs[0])
		ev := d.Events[id]
		if refErr != nil || ev == nil || ev.SessionID != stored.Session {
			return nil, fmt.Errorf("state %s: source is missing or inconsistent", stored.ID)
		}
		validStates = append(validStates, stateMemory(stored.ID, stored.CreatedAtUTC, stored.StateKey, *stored.Scope, ev))
		seenState[stored.ID] = true
	}
	d.States = validStates
	return d, nil
}

// EpisodeID is deterministic from the session and first event, so a rebuild
// recreates the same episode identity.
func EpisodeID(session, firstEvent string) string {
	sum := sha256.Sum256([]byte(session + "\x00" + firstEvent + "\x00" + record.ExtractionVersion))
	return "ep_" + hex.EncodeToString(sum[:8])
}

// MemoryID is the memory ID derived from an episode ID.
func MemoryID(episodeID string) string { return "mem_" + strings.TrimPrefix(episodeID, "ep_") }

// planEpisodes groups events not yet in an episode into new episodes.
func planEpisodes(d *Data, flush bool, stamp string) (fresh []*record.Episode, open int) {
	covered := map[string]bool{}
	for _, ep := range d.Episodes {
		for _, id := range ep.EventIDs {
			covered[id] = true
		}
	}
	type group struct {
		ids   []string
		bytes int
	}
	groups := map[string]*group{}
	var sessions []string
	closeGroup := func(session string, g *group) {
		fresh = append(fresh, &record.Episode{
			Schema: record.SchemaEpisode, ID: EpisodeID(session, g.ids[0]), CreatedAtUTC: stamp,
			SessionID: session, EventIDs: slices.Clone(g.ids),
			ExtractionVersion: record.ExtractionVersion,
		})
		delete(groups, session)
	}
	for _, id := range d.EventOrder {
		if covered[id] {
			continue
		}
		ev := d.Events[id]
		g := groups[ev.SessionID]
		if g == nil {
			g = &group{}
			groups[ev.SessionID] = g
			sessions = append(sessions, ev.SessionID)
		}
		g.ids = append(g.ids, id)
		g.bytes += len(ev.Content)
		boundary := ev.Category == record.CategoryTaskBoundary || ev.Category == record.CategorySessionBoundary
		if boundary || len(g.ids) >= MaxEpisodeEvents || g.bytes >= MaxEpisodeBytes {
			closeGroup(ev.SessionID, g)
		}
	}
	for _, s := range sessions {
		if g := groups[s]; g != nil {
			if flush {
				closeGroup(s, g)
			} else {
				open += len(g.ids)
			}
		}
	}
	return fresh, open
}

var (
	pathPattern  = regexp.MustCompile(`[A-Za-z0-9_@.+-]+(?:/[A-Za-z0-9_@.+-]+)+\.[A-Za-z0-9]{1,8}|\b[A-Za-z0-9_-]+\.(?:go|ts|tsx|js|jsx|py|rs|java|rb|c|h|cc|cpp|md|json|ya?ml|toml|sh|sql)\b`)
	errorPattern = regexp.MustCompile(`(?i)\b(?:error|failed|failure|panic|exception|cannot|undefined)\b`)
	spaceRun     = regexp.MustCompile(`\s+`)
)

// oneLine collapses whitespace and bounds length on a rune boundary.
func oneLine(s string, maxRunes int) string {
	s = strings.TrimSpace(spaceRun.ReplaceAllString(s, " "))
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	r := []rune(s)
	return string(r[:maxRunes-1]) + "…"
}

func addUnique(list []string, v string) []string {
	if v == "" || len(list) >= MaxListItems || slices.Contains(list, v) {
		return list
	}
	return append(list, v)
}

// Extract builds the memory for one episode, or nil when the episode has no
// user or assistant text and no error line worth remembering.
func Extract(ep *record.Episode, d *Data) *record.Memory {
	var goal, resolution string
	var files, errs []string
	var observed, missingSource bool
	refs := []string{record.EpisodeRef(ep.ID)}
	for _, id := range ep.EventIDs {
		ev, ok := d.Events[id]
		if !ok || ev.SessionID != ep.SessionID {
			missingSource = true
			continue
		}
		refs = append(refs, record.EventRef(id))
		switch ev.Category {
		case record.CategoryUserMessage:
			if goal == "" {
				goal = oneLine(ev.Content, MaxFieldRunes)
			}
		case record.CategoryAssistantMessage:
			if t := oneLine(ev.Content, MaxFieldRunes); t != "" {
				resolution = t
			}
		case record.CategoryToolCall, record.CategoryToolResult:
			observed = observed || (ev.Category == record.CategoryToolResult && ev.IsError != nil)
			for _, line := range strings.Split(ev.Content, "\n") {
				if errorPattern.MatchString(line) {
					errs = addUnique(errs, oneLine(line, 200))
				}
			}
		}
		for _, m := range pathPattern.FindAllString(ev.Content, -1) {
			if !strings.HasPrefix(m, ".curator") && !strings.Contains(m, "/.curator/") {
				files = addUnique(files, m)
			}
		}
	}
	if goal == "" && resolution == "" && len(errs) == 0 && !observed && !missingSource {
		return nil
	}
	var kept []string
	for _, t := range slices.Concat([]string{goal, resolution}, files, errs) {
		if t != "" {
			kept = append(kept, t)
		}
	}
	m := &record.Memory{
		Schema: record.SchemaMemory, ID: MemoryID(ep.ID), CreatedAtUTC: ep.CreatedAtUTC,
		Kind: "episode", Status: StatusActive, Retention: RetentionDefault,
		Goal: goal, Summary: goal, Resolution: resolution,
		Files: files, Errors: errs, Session: ep.SessionID,
		ExtractionVersion: record.ExtractionVersion,
		SearchText:        oneLine(strings.Join(kept, " "), MaxSearchRunes),
		SourceRefs:        refs,
		Outcome:           "unresolved",
	}
	if resolution != "" {
		m.ResolutionKind = "assistant_statement"
	}
	addEvidence(ep, d, m)
	return m
}

// Memories derives every episode's memory (before lifecycle state), keyed by ID.
func Memories(d *Data) map[string]*record.Memory {
	out := make(map[string]*record.Memory, len(d.Episodes))
	for _, ep := range d.Episodes {
		if m := Extract(ep, d); m != nil {
			out[m.ID] = m
		}
	}
	for _, state := range d.States {
		out[state.ID] = state
	}
	return out
}

// derive applies lifecycle state to the memories and renders the two views.
func derive(d *Data) (active, superseded [][]byte, sum Summary, err error) {
	sum.Episodes = len(d.Episodes)
	states := record.Replay(d.Lifecycle)
	Visit(d, func(m *record.Memory) bool {
		st := states[m.ID]
		if st.Pinned {
			m.Retention = RetentionPinned
		}
		if st.Archived {
			sum.Archived++
			return true
		}
		if st.SupersededBy != "" {
			m.Status = StatusSuperseded
		}
		var line []byte
		line, err = record.Encode(*m)
		if err != nil {
			return false
		}
		if m.Status == StatusSuperseded {
			superseded = append(superseded, line)
			sum.Superseded++
		} else {
			active = append(active, line)
			sum.Active++
		}
		return true
	})
	if err != nil {
		return nil, nil, sum, err
	}
	return active, superseded, sum, nil
}

// Plan reports what Rebuild would do without writing anything. It takes no lock.
func Plan(j *journal.Journal, opts Options) (Summary, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	d, err := Load(j)
	if err != nil {
		return Summary{}, err
	}
	fresh, open := planEpisodes(d, opts.Flush, record.FormatTime(opts.Now()))
	d.Episodes = append(d.Episodes, fresh...)
	_, _, sum, err := derive(d)
	sum.NewEpisodes, sum.OpenEvents = len(fresh), open
	return sum, err
}

// Rebuild constructs any new episodes, appends them to the journal, derives
// memories from every episode, applies lifecycle state, and atomically
// publishes the views. It is idempotent: with no new events, nothing is
// appended and the views are rewritten byte-identically.
func Rebuild(h *lock.Held, j *journal.Journal, storeDir string, opts Options) (Summary, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	d, err := Load(j)
	if err != nil {
		return Summary{}, err
	}
	fresh, open := planEpisodes(d, opts.Flush, record.FormatTime(opts.Now()))
	if len(fresh) > 0 {
		batch := make([]interface{ Validate() error }, len(fresh))
		for i, ep := range fresh {
			batch[i] = *ep
		}
		if err := j.Append(h, batch...); err != nil {
			return Summary{}, fmt.Errorf("append episodes: %w", err)
		}
		d.Episodes = append(d.Episodes, fresh...)
	}
	active, superseded, sum, err := derive(d)
	sum.NewEpisodes, sum.OpenEvents = len(fresh), open
	if err != nil {
		return sum, err
	}
	dir := filepath.Join(storeDir, "memory")
	if err := WriteAtomic(h, filepath.Join(dir, ActiveFile), joinLines(active)); err != nil {
		return sum, err
	}
	if err := WriteAtomic(h, filepath.Join(dir, SupersededFile), joinLines(superseded)); err != nil {
		return sum, err
	}
	return sum, nil
}

func joinLines(lines [][]byte) []byte {
	if len(lines) == 0 {
		return nil
	}
	return append(bytes.Join(lines, []byte("\n")), '\n')
}

// WriteAtomic replaces path with data so concurrent readers see the old or
// the new complete file, never a partial one: write a temporary sibling,
// fsync, rename over the target, fsync the directory. On failure the target
// is untouched and the temporary file is removed. The *lock.Held argument
// proves the caller owns the exclusive memory lock.
func WriteAtomic(_ *lock.Held, path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary view: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			if rmErr := os.Remove(tmp); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("remove %s: %w", tmp, rmErr))
			}
		}
	}()
	if _, err = f.Write(data); err != nil {
		return errors.Join(fmt.Errorf("write %s: %w", path, err), f.Close())
	}
	if err = f.Chmod(0o600); err != nil {
		return errors.Join(fmt.Errorf("chmod %s: %w", path, err), f.Close())
	}
	if err = f.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync %s: %w", path, err), f.Close())
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	if err = os.Rename(tmp, path); err != nil {
		return fmt.Errorf("publish %s: %w", path, err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open %s for sync: %w", dir, err)
	}
	return errors.Join(d.Sync(), d.Close())
}
