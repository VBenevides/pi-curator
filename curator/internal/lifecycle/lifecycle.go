// Package lifecycle applies explicit memory lifecycle changes (pin, unpin,
// supersede, archive, restore) and offers read-only inspection. Mutations run
// under the caller's exclusive lock; inspection never locks or writes.
//
// Raw events are never removed: archiving moves only a derived memory out of
// the active view, after a compressed snapshot is durably stored. Restore
// verifies that snapshot before re-activating, so the original creation time
// survives the round trip.
package lifecycle

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"pi-curator/curator/internal/journal"
	"pi-curator/curator/internal/lock"
	"pi-curator/curator/internal/memory"
	"pi-curator/curator/internal/record"
)

// ArchiveDir is the cold-storage directory under the store.
const ArchiveDir = "archive"

// maxArchiveBytes bounds how much a restore decompresses.
const maxArchiveBytes = 4 << 20

// Result describes one mutation.
type Result struct {
	Action  string         `json:"action"`
	Target  string         `json:"target"`
	Changed bool           `json:"changed"`
	Summary memory.Summary `json:"summary"`
}

// ArchivePath is where a memory's compressed snapshot lives.
func ArchivePath(storeDir, id string) string {
	return filepath.Join(storeDir, ArchiveDir, id+".jsonl.gz")
}

func newID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate lifecycle id: %w", err)
	}
	return "lc_" + hex.EncodeToString(b[:]), nil
}

// Apply validates and persists one lifecycle change, then republishes the
// views. It is idempotent: a change that is already in effect appends nothing
// (Changed=false) but still republishes, so an interrupted earlier call is
// completed by repeating it.
func Apply(h *lock.Held, j *journal.Journal, storeDir, action, target, by, reason string, now time.Time) (Result, error) {
	res := Result{Action: action, Target: target}
	d, err := memory.Load(j)
	if err != nil {
		return res, err
	}
	mems := memory.Memories(d)
	mem, ok := mems[target]
	if !ok {
		return res, fmt.Errorf("unknown memory %q", target)
	}
	state := record.Replay(d.Lifecycle)[target]
	rec := record.Lifecycle{
		Schema: record.SchemaLifecycle, CreatedAtUTC: record.FormatTime(now),
		Action: action, Target: target, SupersededBy: by, Reason: reason,
	}

	switch action {
	case record.ActionPin:
		res.Changed = !state.Pinned
	case record.ActionUnpin:
		res.Changed = state.Pinned
	case record.ActionArchive:
		if state.SupersededBy != "" {
			return res, fmt.Errorf("memory %s is superseded; superseded facts are already excluded from active memory", target)
		}
		if err := writeArchive(h, storeDir, mem); err != nil {
			return res, err
		}
		res.Changed = !state.Archived
	case record.ActionRestore:
		if err := verifyArchive(storeDir, mem); err != nil {
			return res, err
		}
		res.Changed = state.Archived
	case record.ActionSupersede:
		if _, ok := mems[by]; !ok {
			return res, fmt.Errorf("unknown replacement memory %q", by)
		}
		replacement := mems[by]
		if mem.Kind == "state" || replacement.Kind == "state" {
			if mem.Kind != "state" || replacement.Kind != "state" || mem.StateKey != replacement.StateKey || *mem.Scope != *replacement.Scope {
				return res, errors.New("state supersession requires the same key and declared scope")
			}
		}
		if by == target {
			return res, errors.New("a memory cannot supersede itself")
		}
		if cycle(record.Replay(d.Lifecycle), by, target) {
			return res, fmt.Errorf("memory %s is already superseded by %s; supersession would form a cycle", by, target)
		}
		if state.SupersededBy != "" && state.SupersededBy != by {
			return res, fmt.Errorf("memory %s is already superseded by %s", target, state.SupersededBy)
		}
		res.Changed = state.SupersededBy == ""
	default:
		return res, fmt.Errorf("unsupported action %q", action)
	}

	if res.Changed {
		if rec.ID, err = newID(); err != nil {
			return res, err
		}
		if err := j.Append(h, rec); err != nil {
			return res, fmt.Errorf("journal %s: %w", action, err)
		}
	}
	res.Summary, err = memory.Rebuild(h, j, storeDir, memory.Options{Now: func() time.Time { return now }})
	if err != nil {
		return res, fmt.Errorf("republish views after %s (change %s; repeat the command to retry publication): %w",
			action, map[bool]string{true: "recorded", false: "not needed"}[res.Changed], err)
	}
	return res, nil
}

// cycle reports whether following supersession links from start reaches goal.
func cycle(states map[string]record.State, start, goal string) bool {
	for seen := 0; seen <= len(states); seen++ {
		next := states[start].SupersededBy
		if next == "" {
			return false
		}
		if next == goal {
			return true
		}
		start = next
	}
	return true
}

func writeArchive(h *lock.Held, storeDir string, m *record.Memory) error {
	line, err := record.Encode(*m)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("compress archive: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("compress archive: %w", err)
	}
	return memory.WriteAtomic(h, ArchivePath(storeDir, m.ID), buf.Bytes())
}

// ReadArchive decodes a memory snapshot from cold storage.
func ReadArchive(storeDir, id string) (*record.Memory, error) {
	path := ArchivePath(storeDir, id)
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("archive for %s is not available: %w", id, err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("archive %s is corrupt: %w", path, err)
	}
	data, err := io.ReadAll(io.LimitReader(zr, maxArchiveBytes+1))
	if err != nil {
		return nil, fmt.Errorf("archive %s is corrupt: %w", path, err)
	}
	if len(data) > maxArchiveBytes {
		return nil, fmt.Errorf("archive %s exceeds %d bytes", path, maxArchiveBytes)
	}
	kind, rec, err := record.Decode(bytes.TrimSpace(data))
	if err != nil {
		return nil, fmt.Errorf("archive %s: %w", path, err)
	}
	m, ok := rec.(*record.Memory)
	if !ok || m.ID != id {
		return nil, fmt.Errorf("archive %s holds %s %v, expected memory %s", path, kind, rec, id)
	}
	return m, nil
}

// verifyArchive checks the stored snapshot still matches the memory derived
// from the journal, including its original creation time.
func verifyArchive(storeDir string, want *record.Memory) error {
	got, err := ReadArchive(storeDir, want.ID)
	if err != nil {
		return err
	}
	a, _ := record.Encode(*got)
	b, _ := record.Encode(*want)
	if !bytes.Equal(a, b) {
		return fmt.Errorf("archive for %s does not match the journal-derived memory; refusing to restore", want.ID)
	}
	return nil
}
