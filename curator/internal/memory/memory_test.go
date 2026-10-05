package memory

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pi-curator/curator/internal/journal"
	"pi-curator/curator/internal/lock"
	"pi-curator/curator/internal/record"
)

const stamp = "2026-09-18T14:22:01Z"

var now = func() time.Time { t, _ := time.Parse(time.RFC3339, stamp); return t }

type store struct {
	dir string
	j   *journal.Journal
}

func newStore(t *testing.T) *store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".curator")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return &store{dir: dir, j: journal.New(dir)}
}

func (s *store) with(t *testing.T, fn func(h *lock.Held)) {
	t.Helper()
	if err := lock.With(filepath.Join(s.dir, lock.FileName), time.Second, func(h *lock.Held) error { fn(h); return nil }); err != nil {
		t.Fatal(err)
	}
}

func (s *store) add(t *testing.T, recs ...interface{ Validate() error }) {
	t.Helper()
	s.with(t, func(h *lock.Held) {
		if err := s.j.Append(h, recs...); err != nil {
			t.Fatal(err)
		}
	})
}

func ev(id, session, cat, content string) record.Event {
	return record.Event{Schema: record.SchemaEvent, ID: id, CreatedAtUTC: "2026-09-18T10:00:00Z",
		SessionID: session, Category: cat, Content: content, PolicyVersion: record.PolicyVersion}
}

func (s *store) rebuild(t *testing.T, flush bool) Summary {
	t.Helper()
	var sum Summary
	s.with(t, func(h *lock.Held) {
		var err error
		if sum, err = Rebuild(h, s.j, s.dir, Options{Now: now, Flush: flush}); err != nil {
			t.Fatal(err)
		}
	})
	return sum
}

func (s *store) view(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.dir, "memory", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func sample(t *testing.T) *store {
	s := newStore(t)
	s.add(t,
		ev("e1", "s1", record.CategoryUserMessage, "Fix the failing\nparser test in internal/parse/lexer.go"),
		ev("e2", "s1", record.CategoryToolResult, "ok\nFAIL: lexer error: unexpected token at internal/parse/lexer.go:12"),
		ev("e3", "s1", record.CategoryAssistantMessage, "Fixed the lexer by handling EOF."),
		ev("e4", "s1", record.CategoryTaskBoundary, "done"),
	)
	return s
}

func TestEpisodeClosedAtBoundaryYieldsExtractiveMemory(t *testing.T) {
	s := sample(t)
	sum := s.rebuild(t, false)
	if sum.NewEpisodes != 1 || sum.Active != 1 {
		t.Fatalf("summary %+v", sum)
	}
	line := s.view(t, ActiveFile)
	if strings.Count(line, "\n") != 1 || strings.HasSuffix(line, "\n\n") {
		t.Fatalf("not one JSON object per line: %q", line)
	}
	kind, rec, err := record.Decode([]byte(strings.TrimSpace(line)))
	if err != nil || kind != "memory" {
		t.Fatalf("decode: %v %v", kind, err)
	}
	m := rec.(*record.Memory)
	if len(m.Files) != 1 || m.Files[0] != "internal/parse/lexer.go" {
		t.Fatalf("files %v", m.Files)
	}
	if len(m.Errors) != 1 || !strings.Contains(m.Errors[0], "unexpected token") {
		t.Fatalf("errors %v", m.Errors)
	}
	if m.CreatedAtUTC != stamp || m.ExtractionVersion != record.ExtractionVersion {
		t.Fatalf("provenance %+v", m)
	}
	d, _ := Load(s.j)
	if err := m.ResolveRefs(func(kind, id string) bool {
		if kind == record.RefEvent {
			return d.Events[id] != nil
		}
		for _, ep := range d.Episodes {
			if ep.ID == id {
				return true
			}
		}
		return false
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenGroupWaitsUntilFlushThenRebuildIsIdempotent(t *testing.T) {
	s := newStore(t)
	s.add(t, ev("e1", "s1", record.CategoryUserMessage, "open work"))
	sum := s.rebuild(t, false)
	if sum.NewEpisodes != 0 || sum.OpenEvents != 1 || sum.Active != 0 {
		t.Fatalf("%+v", sum)
	}
	if sum = s.rebuild(t, true); sum.NewEpisodes != 1 || sum.Active != 1 {
		t.Fatalf("flush %+v", sum)
	}
	first := s.view(t, ActiveFile)
	j1, _ := os.ReadFile(s.j.Path())
	if sum = s.rebuild(t, true); sum.NewEpisodes != 0 {
		t.Fatalf("repeat %+v", sum)
	}
	if s.view(t, ActiveFile) != first {
		t.Fatal("view changed on idle rebuild")
	}
	if j2, _ := os.ReadFile(s.j.Path()); !bytes.Equal(j1, j2) {
		t.Fatal("journal grew on idle rebuild")
	}
}

func TestEpisodesAreBoundedAndSessionsSeparate(t *testing.T) {
	s := newStore(t)
	var recs []interface{ Validate() error }
	for i := range MaxEpisodeEvents + 5 {
		recs = append(recs, ev(fmt.Sprintf("a%03d", i), "sA", record.CategoryToolResult, "noise"))
	}
	recs = append(recs, ev("b1", "sB", record.CategoryUserMessage, "other session"))
	recs = append(recs, ev("big", "sC", record.CategoryToolResult, strings.Repeat("x", MaxEpisodeBytes)))
	s.add(t, recs...)
	sum := s.rebuild(t, true)
	if sum.NewEpisodes != 4 { // sA full, sA tail, sB, sC (size cap)
		t.Fatalf("%+v", sum)
	}
	d, _ := Load(s.j)
	for _, ep := range d.Episodes {
		if len(ep.EventIDs) > MaxEpisodeEvents {
			t.Fatalf("episode of %d events", len(ep.EventIDs))
		}
		for _, id := range ep.EventIDs {
			if d.Events[id].SessionID != ep.SessionID {
				t.Fatalf("episode %s mixes sessions", ep.ID)
			}
		}
	}
	// Noise-only episodes stay in the journal but publish no memory.
	if sum.Active != 1 {
		t.Fatalf("active %d", sum.Active)
	}
}

func TestLifecycleStateShapesViews(t *testing.T) {
	s := sample(t)
	s.add(t, ev("f1", "s2", record.CategoryUserMessage, "second task"), ev("f2", "s2", record.CategoryTaskBoundary, "x"),
		ev("g1", "s3", record.CategoryUserMessage, "third task"), ev("g2", "s3", record.CategoryTaskBoundary, "x"))
	s.rebuild(t, false)
	d, _ := Load(s.j)
	if len(d.Episodes) != 3 {
		t.Fatalf("episodes %d", len(d.Episodes))
	}
	ids := make([]string, 3)
	for i, ep := range d.Episodes {
		ids[i] = MemoryID(ep.ID)
	}
	mk := func(id, action, target, by string) interface{ Validate() error } {
		return record.Lifecycle{Schema: record.SchemaLifecycle, ID: id, CreatedAtUTC: stamp, Action: action, Target: target, SupersededBy: by}
	}
	s.add(t, mk("l1", record.ActionPin, ids[0], ""), mk("l2", record.ActionSupersede, ids[1], ids[0]), mk("l3", record.ActionArchive, ids[2], ""))
	sum := s.rebuild(t, false)
	if sum.Active != 1 || sum.Superseded != 1 || sum.Archived != 1 {
		t.Fatalf("%+v", sum)
	}
	if !strings.Contains(s.view(t, ActiveFile), `"retention":"pinned"`) {
		t.Fatal("pin not reflected")
	}
	if !strings.Contains(s.view(t, SupersededFile), `"status":"superseded"`) {
		t.Fatal("supersede not reflected")
	}
	if strings.Contains(s.view(t, ActiveFile), "third task") {
		t.Fatal("archived memory still active")
	}
}

func TestDamagedJournalPublishesNothing(t *testing.T) {
	s := sample(t)
	s.rebuild(t, false)
	before := s.view(t, ActiveFile)
	f, _ := os.OpenFile(s.j.Path(), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("not json\n")
	f.Close()
	s.with(t, func(h *lock.Held) {
		if _, err := Rebuild(h, s.j, s.dir, Options{Now: now}); err == nil {
			t.Fatal("rebuild accepted a damaged journal")
		}
	})
	if s.view(t, ActiveFile) != before {
		t.Fatal("view replaced from a damaged journal")
	}
}

func TestFailedPublishLeavesPreviousViewAndNoTempFiles(t *testing.T) {
	s := sample(t)
	s.rebuild(t, false)
	before := s.view(t, ActiveFile)
	dir := filepath.Join(s.dir, "memory")
	// A directory at the target makes the rename fail after the temp file is written.
	target := filepath.Join(dir, "blocked.jsonl")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	s.with(t, func(h *lock.Held) {
		if err := WriteAtomic(h, target, []byte("x\n")); err == nil {
			t.Fatal("expected publish failure")
		}
	})
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
	if s.view(t, ActiveFile) != before {
		t.Fatal("unrelated view changed")
	}
}

func TestReadersNeverSeePartialView(t *testing.T) {
	s := sample(t)
	s.rebuild(t, false)
	path := filepath.Join(s.dir, "memory", ActiveFile)
	stop := make(chan struct{})
	bad := make(chan string, 1)
	go func() {
		defer close(bad)
		for {
			select {
			case <-stop:
				return
			default:
			}
			b, err := os.ReadFile(path)
			if err != nil || len(b) == 0 || b[len(b)-1] != '\n' {
				bad <- fmt.Sprintf("partial read: %v %q", err, b)
				return
			}
		}
	}()
	for range 200 {
		s.rebuild(t, false)
	}
	close(stop)
	if msg, ok := <-bad; ok {
		t.Fatal(msg)
	}
}
