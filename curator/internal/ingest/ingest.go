// Package ingest turns plugin capture requests into durable journal records.
//
// The stages are strictly ordered: validate, security-filter, then append
// under the exclusive lock. Retention is never consulted: an event is either
// journaled or reported with an explicit non-durable outcome. Curator answers
// synchronously, so it never reports "queued"; durable means fsynced.
package ingest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"time"
	"unicode/utf8"

	"pi-curator/curator/internal/journal"
	"pi-curator/curator/internal/lock"
	"pi-curator/curator/internal/onboard"
	"pi-curator/curator/internal/record"
	"pi-curator/curator/internal/redact"
	"pi-curator/curator/internal/repo"
)

// Limits (NFR-001, NFR-004). Exceeding one is reported per event, never
// silently truncated.
const (
	MaxBatchEvents  = 200
	MaxContentBytes = 256 << 10
	// MaxToolResultBytes bounds stored tool-result text. Longer results are
	// cut after redaction and end with an explicit truncation marker.
	MaxToolResultBytes = 2 << 10
	MaxRequestBytes    = 8 << 20
	MaxPathsPerEvent   = 64
)

// Outcomes reported per event.
const (
	// OutcomeDurable: newly appended and fsynced.
	OutcomeDurable = "durable"
	// OutcomeDuplicate: already durable with identical content (safe retry).
	OutcomeDuplicate = "duplicate"
	// OutcomeRejected: invalid, conflicting, oversized or ineligible; do not retry unchanged.
	OutcomeRejected = "rejected"
	// OutcomeFailed: a system failure prevented journaling; retry is safe.
	OutcomeFailed = "failed"
)

// Machine-readable reason codes.
const (
	CodeInvalid           = "invalid"
	CodeTooLarge          = "event_too_large"
	CodeConflictingID     = "conflicting_id"
	CodeNotReady          = "not_ready"
	CodeMemoryInspection  = "memory_inspection"
	CodeLockTimeout       = "lock_timeout"
	CodeIncompleteTail    = "journal_incomplete_tail"
	CodePolicyInvalid     = "policy_invalid"
	CodeWriteFailed       = "write_failed"
	CodeJournalUnreadable = "journal_unreadable"
)

// EventIn is one captured interaction as sent by the plugin. Creation time is
// assigned by curator unless the caller supplies created_at, an RFC 3339 UTC
// time ending in Z; importers replaying older sessions use it to keep the
// original chronology. created_at_utc stays an unknown field.
type EventIn struct {
	CreatedAt string        `json:"created_at,omitempty"`
	ID        string        `json:"id"`
	SessionID string        `json:"session_id"`
	Category  string        `json:"category"`
	Role      string        `json:"role,omitempty"`
	ToolName  string        `json:"tool_name,omitempty"`
	CallID    string        `json:"call_id,omitempty"`
	Content   string        `json:"content,omitempty"`
	IsError   *bool         `json:"is_error,omitempty"`
	Paths     []string      `json:"paths,omitempty"`
	Source    record.Source `json:"source,omitzero"`
}

// Request is the plugin-to-curator ingestion request.
type Request struct {
	RequestID string    `json:"request_id"`
	Events    []EventIn `json:"events"`
	// Gaps names events the plugin could not deliver (queue overflow, curator
	// down). They are journaled so the loss stays explicit until recovered.
	Gaps []GapIn `json:"gaps,omitempty"`
}

// GapIn identifies one undelivered event.
type GapIn struct {
	SessionID string `json:"session_id"`
	EventID   string `json:"event_id"`
}

// Result is the outcome for one event or gap.
type Result struct {
	ID      string `json:"id"`
	Outcome string `json:"outcome"`
	Code    string `json:"code,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Response lists one result per request event and gap, in request order.
type Response struct {
	RequestID string   `json:"request_id,omitempty"`
	RepoID    string   `json:"repo_id,omitempty"`
	Results   []Result `json:"results"`
	Gaps      []Result `json:"gaps,omitempty"`
	// JournalWarnings reports pre-existing malformed records or an
	// incomplete tail found while ingesting; it does not imply failure.
	JournalWarnings []string `json:"journal_warnings,omitempty"`
}

// Options configures an ingestion run.
type Options struct {
	Now         func() time.Time
	LockTimeout time.Duration
}

// ParseRequest strictly decodes a request, rejecting unknown fields.
func ParseRequest(data []byte) (Request, error) {
	var req Request
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return Request{}, fmt.Errorf("decode request: %w", err)
	}
	if dec.More() {
		return Request{}, errors.New("decode request: trailing data")
	}
	if len(req.Events) == 0 && len(req.Gaps) == 0 {
		return Request{}, errors.New("request has no events or gaps")
	}
	if len(req.Events) > MaxBatchEvents || len(req.Gaps) > MaxBatchEvents {
		return Request{}, fmt.Errorf("request has %d events and %d gaps, limit %d each", len(req.Events), len(req.Gaps), MaxBatchEvents)
	}
	return req, nil
}

var internalActivity = regexp.MustCompile(
	`(?:^|[\s"'=:/\\])\.curator(?:[/\\\s"']|$)|(?:^|[\s"';&|(])(?:\S*/)?curator\s+(?:status|init|ingest|show|list|history|pin|unpin|supersede|archive|restore|feedback|maintain|gaps|recover)\b`)

// ineligible reports memory inspection or curator activity, which must not
// recursively become learned history.
func ineligible(e EventIn) bool {
	if e.Category != record.CategoryToolCall && e.Category != record.CategoryToolResult {
		return false
	}
	for _, p := range e.Paths {
		if internalActivity.MatchString(filepath.ToSlash(p)) {
			return true
		}
	}
	return internalActivity.MatchString(e.Content)
}

// boundResult cuts tool-result text to MaxToolResultBytes on a rune boundary
// and appends a marker with the kept and original sizes. It runs after
// redaction so a secret split by the cut cannot escape its pattern.
func boundResult(category, content string) string {
	if category != record.CategoryToolResult || len(content) <= MaxToolResultBytes {
		return content
	}
	end := MaxToolResultBytes
	for end > 0 && !utf8.RuneStart(content[end]) {
		end--
	}
	return fmt.Sprintf("%s\n[truncated: kept %d of %d bytes]", content[:end], end, len(content))
}

func validate(e EventIn) (code, msg string) {
	switch {
	case !record.ValidID(e.ID):
		return CodeInvalid, "invalid id"
	case !record.ValidID(e.SessionID):
		return CodeInvalid, "invalid session_id"
	case !record.IsEventCategory(e.Category):
		return CodeInvalid, fmt.Sprintf("unsupported category %q", e.Category)
	case e.CreatedAt != "" && record.ValidTimestamp(e.CreatedAt) != nil:
		return CodeInvalid, fmt.Sprintf("created_at %q is not an RFC 3339 UTC time ending in Z", e.CreatedAt)
	case len(e.Content) > MaxContentBytes:
		return CodeTooLarge, fmt.Sprintf("content is %d bytes, limit %d", len(e.Content), MaxContentBytes)
	case len(e.Paths) > MaxPathsPerEvent:
		return CodeInvalid, fmt.Sprintf("%d paths, limit %d", len(e.Paths), MaxPathsPerEvent)
	case e.IsError != nil && (e.Category != record.CategoryToolResult || e.CallID == ""):
		return CodeInvalid, "is_error needs a linked tool_result"
	}
	return "", ""
}

// contentHash identifies an event's stored payload for retry/conflict checks.
// It is recomputed from the stored fields, so it is never persisted.
func contentHash(ev record.Event) string {
	status := ""
	if ev.IsError != nil {
		status = "tool_succeeded"
		if *ev.IsError {
			status = "tool_failed"
		}
	}
	b, _ := json.Marshal([]string{ev.SessionID, ev.Category, ev.Role, ev.ToolName, ev.CallID, ev.Content, status})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type pending struct {
	idx  int
	ev   record.Event
	hash string // contentHash(ev)
}

// Run ingests the request for the repository containing dir.
func Run(dir string, req Request, opts Options) Response {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	resp := Response{RequestID: req.RequestID, Results: make([]Result, len(req.Events)), Gaps: make([]Result, len(req.Gaps))}
	for i, g := range req.Gaps {
		if !record.ValidID(g.SessionID) || !record.ValidID(g.EventID) {
			resp.Gaps[i] = Result{ID: GapID(g.SessionID, g.EventID), Outcome: OutcomeRejected, Code: CodeInvalid, Error: "invalid session_id or event_id"}
		}
	}
	failAll := func(outcome, code string, err error) Response {
		for i, e := range req.Events {
			if resp.Results[i].Outcome == "" {
				resp.Results[i] = Result{ID: e.ID, Outcome: outcome, Code: code, Error: err.Error()}
			}
		}
		for i, g := range req.Gaps {
			if resp.Gaps[i].Outcome == "" {
				resp.Gaps[i] = Result{ID: GapID(g.SessionID, g.EventID), Outcome: outcome, Code: code, Error: err.Error()}
			}
		}
		return resp
	}

	info, err := repo.Resolve(dir)
	if err != nil {
		return failAll(OutcomeRejected, CodeNotReady, err)
	}
	st, err := onboard.Inspect(dir)
	if err != nil {
		return failAll(OutcomeFailed, CodeNotReady, err)
	}
	if st.State != onboard.StateReady {
		return failAll(OutcomeRejected, CodeNotReady, fmt.Errorf("memory is not ready (state %s)", st.State))
	}
	repoID, err := info.ID()
	if err != nil {
		return failAll(OutcomeFailed, CodeNotReady, err)
	}
	resp.RepoID = repoID
	policy, err := redact.Load(info.StoreDir())
	if err != nil {
		return failAll(OutcomeFailed, CodePolicyInvalid, err)
	}

	var todo []pending
	for i, in := range req.Events {
		res := Result{ID: in.ID}
		if code, msg := validate(in); code != "" {
			res.Outcome, res.Code, res.Error = OutcomeRejected, code, msg
		} else if ineligible(in) {
			res.Outcome, res.Code, res.Error = OutcomeRejected, CodeMemoryInspection, "curator activity and memory inspection are not learned"
		}
		if res.Outcome != "" {
			resp.Results[i] = res
			continue
		}
		filtered := policy.Apply(in.Content, in.Paths, in.Category == record.CategoryToolCall || in.Category == record.CategoryToolResult)
		ev := record.Event{
			Schema: record.SchemaEvent, ID: in.ID, CreatedAtUTC: in.CreatedAt, SessionID: in.SessionID,
			Category: in.Category, Role: in.Role, ToolName: in.ToolName, CallID: in.CallID,
			Content: boundResult(in.Category, filtered.Content), Redactions: filtered.Redactions, PolicyVersion: record.PolicyVersion, Source: in.Source,
			IsError: in.IsError,
		}
		todo = append(todo, pending{idx: i, ev: ev, hash: contentHash(ev)})
	}
	if len(todo) == 0 && len(req.Gaps) == 0 {
		return resp
	}

	jr := journal.New(info.StoreDir())
	err = lock.With(filepath.Join(info.StoreDir(), lock.FileName), opts.LockTimeout, func(h *lock.Held) error {
		return commit(h, jr, todo, req.Gaps, &resp, opts.Now())
	})
	if err != nil {
		code := CodeWriteFailed
		if errors.Is(err, lock.ErrTimeout) {
			code = CodeLockTimeout
		}
		// Anything not already decided inside the lock did not become durable.
		for _, p := range todo {
			if resp.Results[p.idx].Outcome == "" || resp.Results[p.idx].Outcome == OutcomeDurable {
				resp.Results[p.idx] = Result{ID: p.ev.ID, Outcome: OutcomeFailed, Code: code, Error: err.Error()}
			}
		}
		for i, g := range req.Gaps {
			if resp.Gaps[i].Outcome == "" || resp.Gaps[i].Outcome == OutcomeDurable {
				resp.Gaps[i] = Result{ID: GapID(g.SessionID, g.EventID), Outcome: OutcomeFailed, Code: code, Error: err.Error()}
			}
		}
	}
	return resp
}

// GapStage is the Progress.Stage of a capture-gap record.
const GapStage = "capture_gap"

// GapID is the deterministic record ID for a reported gap.
func GapID(session, event string) string {
	sum := sha256.Sum256([]byte(session + "\x00" + event))
	return "gap_" + hex.EncodeToString(sum[:8])
}

// GapCursor is the Progress.Cursor identifying the missing event.
func GapCursor(session, event string) string { return session + "/" + event }

func commit(h *lock.Held, jr *journal.Journal, todo []pending, gaps []GapIn, resp *Response, now time.Time) error {
	want := make(map[string]string, len(todo)) // id -> hash for ids in this request
	for _, p := range todo {
		want[p.ev.ID] = ""
	}
	existing := make(map[string]string)
	knownGaps := make(map[string]bool)
	rep, err := jr.Scan(func(e journal.Entry) error {
		switch r := e.Rec.(type) {
		case *record.Event:
			if _, wanted := want[r.ID]; wanted {
				existing[r.ID] = contentHash(*r)
			}
		case *record.Progress:
			if r.Stage == GapStage {
				knownGaps[r.ID] = true
			}
		}
		return nil
	})
	failAll := func(code, msg string) {
		for _, p := range todo {
			resp.Results[p.idx] = Result{ID: p.ev.ID, Outcome: OutcomeFailed, Code: code, Error: msg}
		}
		for i, g := range gaps {
			if resp.Gaps[i].Outcome == OutcomeRejected {
				continue
			}
			resp.Gaps[i] = Result{ID: GapID(g.SessionID, g.EventID), Outcome: OutcomeFailed, Code: code, Error: msg}
		}
	}
	if err != nil {
		failAll(CodeJournalUnreadable, err.Error())
		return nil
	}
	for _, m := range rep.Malformed {
		resp.JournalWarnings = append(resp.JournalWarnings, "malformed record: "+m.String())
	}
	if rep.MalformedOverrun {
		resp.JournalWarnings = append(resp.JournalWarnings, fmt.Sprintf("more than %d malformed records", journal.MaxProblems))
	}
	if rep.Tail != nil {
		resp.JournalWarnings = append(resp.JournalWarnings, "incomplete tail: "+rep.Tail.String())
		failAll(CodeIncompleteTail, "journal ends with an incomplete record; run `curator recover` before ingesting")
		return nil
	}

	stamp := record.FormatTime(now)
	var batch []interface{ Validate() error }
	batched := map[string]bool{}
	for _, p := range todo {
		prev, seen := existing[p.ev.ID]
		switch {
		case !seen:
			if p.ev.CreatedAtUTC == "" {
				p.ev.CreatedAtUTC = stamp
			}
			batch = append(batch, p.ev)
			batched[p.ev.ID] = true
			existing[p.ev.ID] = p.hash // later same-ID entries in this batch
			resp.Results[p.idx] = Result{ID: p.ev.ID, Outcome: OutcomeDurable}
		case prev == p.hash:
			resp.Results[p.idx] = Result{ID: p.ev.ID, Outcome: OutcomeDuplicate}
		default:
			resp.Results[p.idx] = Result{ID: p.ev.ID, Outcome: OutcomeRejected, Code: CodeConflictingID,
				Error: "event id already journaled with different content"}
		}
	}
	gapBatched := map[int]bool{}
	for i, g := range gaps {
		id := GapID(g.SessionID, g.EventID)
		if resp.Gaps[i].Outcome == OutcomeRejected {
			continue
		}
		if knownGaps[id] {
			resp.Gaps[i] = Result{ID: id, Outcome: OutcomeDuplicate}
			continue
		}
		knownGaps[id] = true
		batch = append(batch, record.Progress{Schema: record.SchemaProgress, ID: id, CreatedAtUTC: stamp,
			Stage: GapStage, Cursor: GapCursor(g.SessionID, g.EventID), Version: record.ExtractionVersion})
		gapBatched[i] = true
		resp.Gaps[i] = Result{ID: id, Outcome: OutcomeDurable}
	}
	if len(batch) == 0 {
		return nil
	}
	if err := jr.Append(h, batch...); err != nil {
		for _, p := range todo {
			if batched[p.ev.ID] && resp.Results[p.idx].Outcome != OutcomeRejected {
				resp.Results[p.idx] = Result{ID: p.ev.ID, Outcome: OutcomeFailed, Code: CodeWriteFailed, Error: err.Error()}
			}
		}
		for i := range gapBatched {
			resp.Gaps[i] = Result{ID: resp.Gaps[i].ID, Outcome: OutcomeFailed, Code: CodeWriteFailed, Error: err.Error()}
		}
	}
	return nil
}
