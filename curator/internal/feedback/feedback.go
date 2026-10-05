// Package feedback records explicit, source-backed labels about episodes.
//
// It records only what a user, a task outcome or a benchmark run explicitly
// states. Nothing here infers usefulness from reads: ordinary filesystem
// reads give no exposure signal, and there are no recall counters. Unknown
// outcomes simply have no record. Association is not causation, so labels are
// evidence for later policy work, never an automatic state change.
package feedback

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"pi-curator/curator/internal/journal"
	"pi-curator/curator/internal/lock"
	"pi-curator/curator/internal/memory"
	"pi-curator/curator/internal/record"
	"pi-curator/curator/internal/redact"
)

// Labels and sources accepted by Add.
var (
	Labels  = []string{"useful", "misleading", "outdated", "outcome_success", "outcome_failure"}
	Sources = []string{"user_correction", "task_outcome", "benchmark"}
)

// MaxNoteRunes bounds the free-text note.
const MaxNoteRunes = 500

// Result describes one Add call.
type Result struct {
	ID      string `json:"id"`
	Episode string `json:"episode_id"`
	Changed bool   `json:"changed"`
}

// ID is deterministic from the content, so repeating the same statement is a
// no-op instead of inflating evidence.
func ID(episode, label, source, note string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{episode, label, source, note}, "\x00")))
	return "fb_" + hex.EncodeToString(sum[:8])
}

// Add validates and appends one feedback record under the caller's lock. The
// episode may be given as an episode ID or the memory ID derived from it.
func Add(h *lock.Held, j *journal.Journal, policy *redact.Policy, target, label, source, note string, now time.Time) (Result, error) {
	if !slices.Contains(Labels, label) {
		return Result{}, fmt.Errorf("unsupported label %q (use %s)", label, strings.Join(Labels, ", "))
	}
	if !slices.Contains(Sources, source) {
		return Result{}, fmt.Errorf("unsupported source %q (use %s)", source, strings.Join(Sources, ", "))
	}
	if utf8.RuneCountInString(note) > MaxNoteRunes {
		return Result{}, fmt.Errorf("note exceeds %d characters", MaxNoteRunes)
	}
	d, err := memory.Load(j)
	if err != nil {
		return Result{}, err
	}
	episode := ""
	for _, ep := range d.Episodes {
		if ep.ID == target || memory.MemoryID(ep.ID) == target {
			episode = ep.ID
		}
	}
	if episode == "" {
		return Result{}, fmt.Errorf("unknown episode or memory %q", target)
	}
	note = policy.Apply(note, nil, false).Content
	res := Result{ID: ID(episode, label, source, note), Episode: episode}
	for _, f := range existing(j) {
		if f == res.ID {
			return res, nil
		}
	}
	rec := record.Feedback{Schema: record.SchemaFeedback, ID: res.ID, CreatedAtUTC: record.FormatTime(now),
		EpisodeID: episode, Label: label, Source: source, Note: note}
	if err := j.Append(h, rec); err != nil {
		return res, fmt.Errorf("journal feedback: %w", err)
	}
	res.Changed = true
	return res, nil
}

func existing(j *journal.Journal) []string {
	var ids []string
	_, _ = j.Scan(func(e journal.Entry) error {
		if f, ok := e.Rec.(*record.Feedback); ok {
			ids = append(ids, f.ID)
		}
		return nil
	})
	return ids
}

// List returns all feedback, oldest first, without locking. episode filters
// when non-empty.
func List(j *journal.Journal, episode string) ([]record.Feedback, error) {
	var out []record.Feedback
	rep, err := j.Scan(func(e journal.Entry) error {
		if f, ok := e.Rec.(*record.Feedback); ok && (episode == "" || f.EpisodeID == episode || memory.MemoryID(f.EpisodeID) == episode) {
			out = append(out, *f)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read journal: %w", err)
	}
	if !rep.Healthy() {
		return nil, fmt.Errorf("journal is damaged; feedback list may be incomplete")
	}
	return out, nil
}
