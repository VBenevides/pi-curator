// Package record defines the versioned JSONL record contracts shared by every
// curator component. Authoritative records (events, episodes, lifecycle,
// progress, model decisions, feedback) are separate from the derived,
// agent-readable Memory view record.
package record

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Schema identifiers. A name and meaning stay stable within a version.
const (
	SchemaEvent         = "curator.event.v2"
	SchemaEpisode       = "curator.episode.v2"
	SchemaLifecycle     = "curator.lifecycle.v1"
	SchemaProgress      = "curator.progress.v1"
	SchemaModelDecision = "curator.model_decision.v1"
	SchemaFeedback      = "curator.feedback.v1"
	SchemaMemory        = "curator.memory.v1"
)

// Event categories: every visible interaction category the plugin captures.
const (
	CategoryUserMessage      = "user_message"
	CategoryAssistantMessage = "assistant_message"
	CategoryToolCall         = "tool_call"
	CategoryToolResult       = "tool_result"
	CategorySessionBoundary  = "session_boundary"
	CategoryTaskBoundary     = "task_boundary"
)

var eventCategories = map[string]bool{
	CategoryUserMessage: true, CategoryAssistantMessage: true, CategoryToolCall: true,
	CategoryToolResult: true, CategorySessionBoundary: true, CategoryTaskBoundary: true,
}

// IsEventCategory reports whether c is a supported event category.
func IsEventCategory(c string) bool { return eventCategories[c] }

// Lifecycle actions.
const (
	ActionPin       = "pin"
	ActionUnpin     = "unpin"
	ActionSupersede = "supersede"
	ActionArchive   = "archive"
	ActionRestore   = "restore"
)

// Versions recorded for deterministic rebuilds.
const (
	ExtractionVersion = "extract.v2"
	PolicyVersion     = "policy.v1"
)

// Source identifies the host entry an event was captured from, so a missing
// record can later be recovered idempotently from host history. The host
// session is the event's session_id and is not repeated here.
type Source struct {
	Host    string `json:"host,omitempty"`
	EntryID string `json:"entry_id,omitempty"`
}

// Event is one eligible visible interaction, stored after security filtering.
type Event struct {
	Schema        string `json:"schema"`
	ID            string `json:"id"`
	CreatedAtUTC  string `json:"created_at_utc"`
	SessionID     string `json:"session_id"`
	Category      string `json:"category"`
	Role          string `json:"role,omitempty"`
	ToolName      string `json:"tool_name,omitempty"`
	CallID        string `json:"call_id,omitempty"`
	Content       string `json:"content,omitempty"`
	IsError       *bool  `json:"is_error,omitempty"`
	Redactions    int    `json:"redactions,omitempty"`
	PolicyVersion string `json:"policy_version"`
	Source        Source `json:"source,omitzero"`
}

// Episode is a bounded group of journaled events, referenced by event ID.
type Episode struct {
	Schema            string   `json:"schema"`
	ID                string   `json:"id"`
	CreatedAtUTC      string   `json:"created_at_utc"`
	SessionID         string   `json:"session_id"`
	EventIDs          []string `json:"event_ids"`
	ExtractionVersion string   `json:"extraction_version"`
}

// Lifecycle is a durable state change targeting a memory (episode) ID.
type Lifecycle struct {
	Schema       string `json:"schema"`
	ID           string `json:"id"`
	CreatedAtUTC string `json:"created_at_utc"`
	Action       string `json:"action"`
	Target       string `json:"target"`
	SupersededBy string `json:"superseded_by,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// Progress records resumable asynchronous work, such as episode construction.
type Progress struct {
	Schema       string `json:"schema"`
	ID           string `json:"id"`
	CreatedAtUTC string `json:"created_at_utc"`
	Stage        string `json:"stage"`
	Cursor       string `json:"cursor"`
	Version      string `json:"version"`
}

// ModelDecision persists a classifier result with the versions needed to
// explain and replay it.
type ModelDecision struct {
	Schema        string             `json:"schema"`
	ID            string             `json:"id"`
	CreatedAtUTC  string             `json:"created_at_utc"`
	EpisodeID     string             `json:"episode_id"`
	ModelVersion  string             `json:"model_version"`
	PolicyVersion string             `json:"policy_version"`
	Scores        map[string]float64 `json:"scores"`
	Action        string             `json:"action"`
}

// Feedback is explicit, supported quality or outcome evidence for an episode.
type Feedback struct {
	Schema       string `json:"schema"`
	ID           string `json:"id"`
	CreatedAtUTC string `json:"created_at_utc"`
	EpisodeID    string `json:"episode_id"`
	Label        string `json:"label"`
	Source       string `json:"source"`
	Note         string `json:"note,omitempty"`
}

// Memory is one compact derived active-view record. CreatedAtUTC is the
// original memory creation time and survives rebuild and archive/restore.
type Memory struct {
	Schema            string    `json:"schema"`
	ID                string    `json:"id"`
	CreatedAtUTC      string    `json:"created_at_utc"`
	Kind              string    `json:"kind,omitempty"`
	Status            string    `json:"status"`
	Retention         string    `json:"retention,omitempty"`
	Goal              string    `json:"goal,omitempty"`
	Summary           string    `json:"summary,omitempty"`
	Resolution        string    `json:"resolution,omitempty"`
	ResolutionKind    string    `json:"resolution_kind,omitempty"`
	Outcome           string    `json:"outcome,omitempty"`
	Files             []string  `json:"files,omitempty"`
	Symbols           []string  `json:"symbols,omitempty"`
	Errors            []string  `json:"errors,omitempty"`
	Session           string    `json:"session"`
	ExtractionVersion string    `json:"extraction_version"`
	SearchText        string    `json:"search_text"`
	SourceRefs        []string  `json:"source_refs"`
	Claims            []Claim   `json:"claims,omitempty"`
	Attempts          []Attempt `json:"attempts,omitempty"`
	Lessons           []Claim   `json:"lessons,omitempty"`
	EvidenceGaps      []string  `json:"evidence_gaps,omitempty"`
	Scope             *Scope    `json:"scope,omitempty"`
	StateKey          string    `json:"state_key,omitempty"`
}

// Scope is an explicitly declared repository/branch namespace, not an inference
// about where a historical event was originally captured. Empty branch is global.
type Scope struct {
	Repository string `json:"repository"`
	Branch     string `json:"branch,omitempty"`
}

// Claim is extractive text or a narrowly stated execution observation.
type Claim struct {
	Kind       string   `json:"kind"`
	Text       string   `json:"text"`
	SourceRefs []string `json:"source_refs"`
}

// Attempt reports host tool status, not task success or causal effectiveness.
type Attempt struct {
	Tool        string   `json:"tool"`
	Input       string   `json:"input,omitempty"`
	Observation string   `json:"observation"`
	SourceRefs  []string `json:"source_refs"`
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,127}$`)

// ValidID reports whether id is an acceptable stable identifier.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// FormatTime renders t as an RFC 3339 UTC timestamp ending in Z.
func FormatTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// ValidTimestamp checks for a parseable RFC 3339 UTC timestamp ending in Z.
func ValidTimestamp(s string) error {
	if !strings.HasSuffix(s, "Z") {
		return fmt.Errorf("created_at_utc %q must end in Z", s)
	}
	if _, err := time.Parse(time.RFC3339, s); err != nil {
		return fmt.Errorf("created_at_utc %q: %w", s, err)
	}
	return nil
}

func check(schema, want, id, created string) error {
	if schema != want {
		return fmt.Errorf("schema %q, want %q", schema, want)
	}
	if !ValidID(id) {
		return fmt.Errorf("invalid id %q", id)
	}
	return ValidTimestamp(created)
}

// Validate checks required fields and invariants.
func (e Event) Validate() error {
	if err := check(e.Schema, SchemaEvent, e.ID, e.CreatedAtUTC); err != nil {
		return err
	}
	switch {
	case !ValidID(e.SessionID):
		return fmt.Errorf("event %s: invalid session_id", e.ID)
	case !IsEventCategory(e.Category):
		return fmt.Errorf("event %s: unsupported category %q", e.ID, e.Category)
	case e.PolicyVersion == "":
		return fmt.Errorf("event %s: missing policy_version", e.ID)
	case e.IsError != nil && (e.Category != CategoryToolResult || e.CallID == ""):
		return fmt.Errorf("event %s: is_error needs a linked tool_result", e.ID)
	}
	return nil
}

// Validate checks required fields and invariants.
func (e Episode) Validate() error {
	if err := check(e.Schema, SchemaEpisode, e.ID, e.CreatedAtUTC); err != nil {
		return err
	}
	if len(e.EventIDs) == 0 {
		return fmt.Errorf("episode %s: no event_ids", e.ID)
	}
	for _, id := range e.EventIDs {
		if !ValidID(id) {
			return fmt.Errorf("episode %s: invalid event id %q", e.ID, id)
		}
	}
	if !ValidID(e.SessionID) || e.ExtractionVersion == "" {
		return fmt.Errorf("episode %s: missing session_id or extraction_version", e.ID)
	}
	return nil
}

// Validate checks required fields and invariants.
func (l Lifecycle) Validate() error {
	if err := check(l.Schema, SchemaLifecycle, l.ID, l.CreatedAtUTC); err != nil {
		return err
	}
	switch l.Action {
	case ActionPin, ActionUnpin, ActionArchive, ActionRestore:
	case ActionSupersede:
		if !ValidID(l.SupersededBy) || l.SupersededBy == l.Target {
			return fmt.Errorf("lifecycle %s: supersede needs a distinct superseded_by", l.ID)
		}
	default:
		return fmt.Errorf("lifecycle %s: unsupported action %q", l.ID, l.Action)
	}
	if !ValidID(l.Target) {
		return fmt.Errorf("lifecycle %s: invalid target", l.ID)
	}
	return nil
}

// Validate checks required fields and invariants.
func (p Progress) Validate() error {
	if err := check(p.Schema, SchemaProgress, p.ID, p.CreatedAtUTC); err != nil {
		return err
	}
	if p.Stage == "" || p.Version == "" {
		return fmt.Errorf("progress %s: missing stage or version", p.ID)
	}
	return nil
}

// Validate checks required fields and invariants.
func (d ModelDecision) Validate() error {
	if err := check(d.Schema, SchemaModelDecision, d.ID, d.CreatedAtUTC); err != nil {
		return err
	}
	if !ValidID(d.EpisodeID) || d.ModelVersion == "" || d.PolicyVersion == "" || d.Action == "" || len(d.Scores) == 0 {
		return fmt.Errorf("model decision %s: missing episode_id, versions, scores or action", d.ID)
	}
	return nil
}

// ResolveRefs verifies every source reference against exists, so a memory
// never cites provenance that is not in the authoritative records.
func (m Memory) ResolveRefs(exists func(kind, id string) bool) error {
	for _, r := range m.SourceRefs {
		kind, id, err := ParseRef(r)
		if err != nil {
			return err
		}
		if !exists(kind, id) {
			return fmt.Errorf("memory %s: unresolved source ref %q", m.ID, r)
		}
	}
	return nil
}

// Validate checks required fields and invariants.
func (f Feedback) Validate() error {
	if err := check(f.Schema, SchemaFeedback, f.ID, f.CreatedAtUTC); err != nil {
		return err
	}
	if !ValidID(f.EpisodeID) || f.Label == "" || f.Source == "" {
		return fmt.Errorf("feedback %s: missing episode_id, label or source", f.ID)
	}
	return nil
}

// Validate checks required fields and invariants, including that the record
// is a single physical line once encoded.
func (m Memory) Validate() error {
	if err := check(m.Schema, SchemaMemory, m.ID, m.CreatedAtUTC); err != nil {
		return err
	}
	if m.Status == "" || m.ExtractionVersion == "" || len(m.SourceRefs) == 0 {
		return fmt.Errorf("memory %s: missing status, extraction_version or source_refs", m.ID)
	}
	if m.Kind == "state" {
		if m.Scope == nil || !ValidID(m.Scope.Repository) || !ValidID(m.StateKey) || len(m.Scope.Branch) > 256 || strings.ContainsAny(m.Scope.Branch, "\x00\r\n") || len(m.SourceRefs) != 1 {
			return fmt.Errorf("memory %s: invalid state namespace or source", m.ID)
		}
		if kind, _, err := ParseRef(m.SourceRefs[0]); err != nil || kind != RefEvent {
			return fmt.Errorf("memory %s: state needs one source event", m.ID)
		}
	}
	if strings.ContainsAny(m.SearchText, "\r\n") {
		return fmt.Errorf("memory %s: search_text spans lines", m.ID)
	}
	for _, r := range m.SourceRefs {
		if _, _, err := ParseRef(r); err != nil {
			return fmt.Errorf("memory %s: %w", m.ID, err)
		}
	}
	for _, claims := range [][]Claim{m.Claims, m.Lessons} {
		for _, claim := range claims {
			if claim.Kind == "" || claim.Text == "" || len(claim.SourceRefs) == 0 {
				return fmt.Errorf("memory %s: incomplete claim", m.ID)
			}
			for _, ref := range claim.SourceRefs {
				if !slices.Contains(m.SourceRefs, ref) {
					return fmt.Errorf("memory %s: claim reference is not a source: %s", m.ID, ref)
				}
			}
		}
	}
	for _, attempt := range m.Attempts {
		if len(attempt.SourceRefs) == 0 || (attempt.Observation != "unknown" && attempt.Observation != "tool_failed" && attempt.Observation != "tool_succeeded") {
			return fmt.Errorf("memory %s: invalid attempt", m.ID)
		}
		for _, ref := range attempt.SourceRefs {
			if !slices.Contains(m.SourceRefs, ref) {
				return fmt.Errorf("memory %s: attempt reference is not a source: %s", m.ID, ref)
			}
		}
	}
	return nil
}

// Source reference kinds.
const (
	RefEvent   = "event"
	RefEpisode = "episode"
)

// EventRef and EpisodeRef build stable provenance references.
func EventRef(id string) string   { return RefEvent + ":" + id }
func EpisodeRef(id string) string { return RefEpisode + ":" + id }

// ParseRef splits a "kind:id" source reference.
func ParseRef(ref string) (kind, id string, err error) {
	kind, id, ok := strings.Cut(ref, ":")
	if !ok || (kind != RefEvent && kind != RefEpisode) || !ValidID(id) {
		return "", "", fmt.Errorf("invalid source ref %q", ref)
	}
	return kind, id, nil
}

// Kind names a decoded record type.
type Kind string

// Decode strictly decodes and validates one JSONL line, returning the typed
// record. Unknown fields and trailing data are rejected.
func Decode(line []byte) (Kind, any, error) {
	var head struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return "", nil, fmt.Errorf("decode record: %w", err)
	}
	var (
		kind Kind
		rec  interface{ Validate() error }
	)
	switch head.Schema {
	case SchemaEvent:
		kind, rec = "event", &Event{}
	case SchemaEpisode:
		kind, rec = "episode", &Episode{}
	case SchemaLifecycle:
		kind, rec = "lifecycle", &Lifecycle{}
	case SchemaProgress:
		kind, rec = "progress", &Progress{}
	case SchemaModelDecision:
		kind, rec = "model_decision", &ModelDecision{}
	case SchemaFeedback:
		kind, rec = "feedback", &Feedback{}
	case SchemaMemory:
		kind, rec = "memory", &Memory{}
	default:
		return "", nil, fmt.Errorf("unknown schema %q", head.Schema)
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(rec); err != nil {
		return "", nil, fmt.Errorf("decode %s: %w", kind, err)
	}
	if dec.More() {
		return "", nil, errors.New("trailing data after record")
	}
	if err := rec.Validate(); err != nil {
		return "", nil, err
	}
	return kind, rec, nil
}

// Encode validates and renders a record as one compact line without a
// trailing newline.
func Encode(rec interface{ Validate() error }) ([]byte, error) {
	if err := rec.Validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("encode record: %w", err)
	}
	return b, nil
}
