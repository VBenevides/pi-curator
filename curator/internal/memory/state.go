package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"pi-curator/curator/internal/journal"
	"pi-curator/curator/internal/lock"
	"pi-curator/curator/internal/record"
)

// Visit preserves episode order followed by explicitly registered facts.
func Visit(d *Data, yield func(*record.Memory) bool) {
	for _, ep := range d.Episodes {
		if m := Extract(ep, d); m != nil && !yield(m) {
			return
		}
	}
	for _, m := range d.States {
		if !yield(m) {
			return
		}
	}
}

func stateMemory(id, stamp, key string, scope record.Scope, ev *record.Event) *record.Memory {
	text := oneLine(ev.Content, MaxFieldRunes)
	refs := []string{record.EventRef(ev.ID)}
	return &record.Memory{Schema: record.SchemaMemory, ID: id, CreatedAtUTC: stamp, Kind: "state", Status: StatusActive, Retention: RetentionDefault, Goal: key, Summary: text, Session: ev.SessionID, ExtractionVersion: record.ExtractionVersion, SearchText: oneLine(key+" "+text, MaxSearchRunes), SourceRefs: refs, Scope: &scope, StateKey: key, Claims: []record.Claim{{Kind: "declared_state", Text: text, SourceRefs: refs}}}
}

// RegisterState declares a namespace for existing redacted evidence. Registration
// never replaces another declaration; only an explicit lifecycle operation does.
func RegisterState(h *lock.Held, j *journal.Journal, storeDir, key string, scope record.Scope, source string, now time.Time) (*record.Memory, error) {
	d, err := Load(j)
	if err != nil {
		return nil, err
	}
	ev := d.Events[source]
	if ev == nil || strings.TrimSpace(ev.Content) == "" {
		return nil, fmt.Errorf("state source %q is missing or empty", source)
	}
	hash := sha256.New()
	for _, part := range []string{scope.Repository, scope.Branch, key, source} {
		hash.Write([]byte(part))
		hash.Write([]byte{0})
	}
	id := "state_" + hex.EncodeToString(hash.Sum(nil)[:16])
	var result *record.Memory
	for _, existing := range d.States {
		if existing.ID == id {
			result = existing
			break
		}
	}
	if result == nil {
		result = stateMemory(id, record.FormatTime(now), key, scope, ev)
		if err := j.Append(h, *result); err != nil {
			return nil, fmt.Errorf("register state: %w", err)
		}
	}
	if _, err := Rebuild(h, j, storeDir, Options{Now: func() time.Time { return now }}); err != nil {
		return nil, fmt.Errorf("state recorded; repeat registration to retry publication: %w", err)
	}
	return result, nil
}
