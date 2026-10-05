package lifecycle

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pi-curator/curator/internal/journal"
	"pi-curator/curator/internal/lock"
	"pi-curator/curator/internal/memory"
	"pi-curator/curator/internal/record"
)

var t0 = time.Date(2026, 9, 18, 14, 22, 1, 0, time.UTC)

type fixture struct {
	dir string
	j   *journal.Journal
	ids []string // memory IDs in episode order
}

func ev(id, session, cat, content string) record.Event {
	return record.Event{Schema: record.SchemaEvent, ID: id, CreatedAtUTC: "2026-09-01T00:00:00Z",
		SessionID: session, Category: cat, Content: content, PolicyVersion: record.PolicyVersion}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".curator")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f := &fixture{dir: dir, j: journal.New(dir)}
	f.with(t, func(h *lock.Held) error {
		if err := f.j.Append(h,
			ev("a1", "sA", record.CategoryUserMessage, "deploy uses blue stack"), ev("a2", "sA", record.CategoryTaskBoundary, "x"),
			ev("b1", "sB", record.CategoryUserMessage, "deploy now uses green stack"), ev("b2", "sB", record.CategoryTaskBoundary, "x"),
			ev("c1", "sC", record.CategoryUserMessage, "unrelated cache work"), ev("c2", "sC", record.CategoryTaskBoundary, "x"),
		); err != nil {
			return err
		}
		_, err := memory.Rebuild(h, f.j, dir, memory.Options{Now: func() time.Time { return t0 }})
		return err
	})
	d, err := memory.Load(f.j)
	if err != nil {
		t.Fatal(err)
	}
	for _, ep := range d.Episodes {
		f.ids = append(f.ids, memory.MemoryID(ep.ID))
	}
	return f
}

func (f *fixture) with(t *testing.T, fn func(h *lock.Held) error) {
	t.Helper()
	if err := lock.With(filepath.Join(f.dir, lock.FileName), time.Second, fn); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) apply(t *testing.T, action, target, by string) Result {
	t.Helper()
	var r Result
	f.with(t, func(h *lock.Held) error {
		var err error
		r, err = Apply(h, f.j, f.dir, action, target, by, "", t0.Add(time.Hour))
		return err
	})
	return r
}

func (f *fixture) active(t *testing.T) string {
	b, err := os.ReadFile(filepath.Join(f.dir, "memory", "active.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestArchiveRestoreRoundTripPreservesOriginalCreationTime(t *testing.T) {
	f := newFixture(t)
	before := f.active(t)
	target := f.ids[2]
	if r := f.apply(t, record.ActionArchive, target, ""); !r.Changed || r.Summary.Archived != 1 {
		t.Fatalf("archive: %+v", r)
	}
	if strings.Contains(f.active(t), "cache work") {
		t.Fatal("archived memory still in active view")
	}
	arch, err := ReadArchive(f.dir, target)
	if err != nil || arch.CreatedAtUTC != "2026-09-18T14:22:01Z" {
		t.Fatalf("archive snapshot: %v %+v", err, arch)
	}
	raw, _ := os.ReadFile(ArchivePath(f.dir, target))
	if len(raw) < 2 || raw[0] != 0x1f || raw[1] != 0x8b {
		t.Fatal("archive is not compressed")
	}
	if r := f.apply(t, record.ActionRestore, target, ""); !r.Changed {
		t.Fatalf("restore: %+v", r)
	}
	if f.active(t) != before {
		t.Fatal("restore did not reproduce the original view (creation time or content changed)")
	}
}

func TestRepeatedMutationsAreIdempotent(t *testing.T) {
	f := newFixture(t)
	for _, a := range []string{record.ActionPin, record.ActionArchive} {
		if !f.apply(t, a, f.ids[0], "").Changed {
			t.Fatalf("first %s unchanged", a)
		}
		size := fileSize(t, f.j.Path())
		if f.apply(t, a, f.ids[0], "").Changed || fileSize(t, f.j.Path()) != size {
			t.Fatalf("repeat %s appended records", a)
		}
	}
	// Restoring an unarchived memory records nothing, and an interrupted
	// restore is completed by repeating it (here: journal says restored).
	f.apply(t, record.ActionRestore, f.ids[0], "")
	size := fileSize(t, f.j.Path())
	if f.apply(t, record.ActionRestore, f.ids[0], "").Changed || fileSize(t, f.j.Path()) != size {
		t.Fatal("repeat restore appended records")
	}
}

func fileSize(t *testing.T, p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

func TestPinSurvivesArchiveRestore(t *testing.T) {
	f := newFixture(t)
	f.apply(t, record.ActionPin, f.ids[0], "")
	f.apply(t, record.ActionArchive, f.ids[0], "")
	f.apply(t, record.ActionRestore, f.ids[0], "")
	got, err := List(f.j, StateActive)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range got {
		if e.Memory.ID == f.ids[0] && e.Pinned && e.Memory.Retention == memory.RetentionPinned {
			return
		}
	}
	t.Fatal("pin lost across archive/restore")
}

func TestSupersedeExcludesOldFactAndKeepsEvidence(t *testing.T) {
	f := newFixture(t)
	f.apply(t, record.ActionSupersede, f.ids[0], f.ids[1])
	active := f.active(t)
	if strings.Contains(active, "blue stack") || !strings.Contains(active, "green stack") {
		t.Fatalf("active view wrong: %s", active)
	}
	det, err := Show(f.j, f.ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if det.State != StateSuperseded || det.SupersededBy != f.ids[1] || len(det.History) != 1 {
		t.Fatalf("detail %+v", det.Entry)
	}
	if len(det.Events) == 0 || det.Events[0].Content != "deploy uses blue stack" || len(det.MissingRefs) != 0 {
		t.Fatalf("evidence not resolved: %+v", det.Events)
	}
	// Conflicts and cycles.
	for _, c := range []struct{ target, by string }{{f.ids[0], f.ids[2]}, {f.ids[1], f.ids[0]}, {f.ids[2], f.ids[2]}, {f.ids[2], "mem_nope"}} {
		var err error
		f.with(t, func(h *lock.Held) error {
			_, err = Apply(h, f.j, f.dir, record.ActionSupersede, c.target, c.by, "", t0)
			return nil
		})
		if err == nil {
			t.Errorf("accepted supersede %s by %s", c.target, c.by)
		}
	}
}

func TestMutationsNeedKnownTargetsAndValidArchives(t *testing.T) {
	f := newFixture(t)
	var err error
	f.with(t, func(h *lock.Held) error {
		_, err = Apply(h, f.j, f.dir, record.ActionPin, "mem_unknown", "", "", t0)
		return nil
	})
	if err == nil {
		t.Fatal("pinned unknown memory")
	}
	f.apply(t, record.ActionArchive, f.ids[0], "")
	// Corrupt archive: restore must refuse and keep the memory archived.
	if err := os.WriteFile(ArchivePath(f.dir, f.ids[0]), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.with(t, func(h *lock.Held) error {
		_, err = Apply(h, f.j, f.dir, record.ActionRestore, f.ids[0], "", "", t0)
		return nil
	})
	if err == nil {
		t.Fatal("restored from a corrupt archive")
	}
	if got, _ := List(f.j, StateArchived); len(got) != 1 {
		t.Fatalf("archived = %d", len(got))
	}
	// Missing archive.
	os.Remove(ArchivePath(f.dir, f.ids[0]))
	f.with(t, func(h *lock.Held) error {
		_, err = Apply(h, f.j, f.dir, record.ActionRestore, f.ids[0], "", "", t0)
		return nil
	})
	if err == nil {
		t.Fatal("restored without an archive")
	}
}

func TestInspectionIsReadOnly(t *testing.T) {
	f := newFixture(t)
	f.apply(t, record.ActionPin, f.ids[0], "")
	snap := snapshot(t, f.dir)
	// Hold the lock: readers must not need it.
	h, err := lock.Acquire(filepath.Join(f.dir, lock.FileName), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	for _, s := range []string{StateAll, StateActive, StateSuperseded, StateArchived} {
		if _, err := List(f.j, s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Show(f.j, f.ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.Plan(f.j, memory.Options{Now: func() time.Time { return t0 }, Flush: true}); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(t, f.dir); after != snap {
		t.Fatalf("read-only inspection changed durable state:\n%s\n---\n%s", snap, after)
	}
	if _, err := List(f.j, "bogus"); err == nil {
		t.Fatal("accepted unknown state")
	}
}

func snapshot(t *testing.T, root string) string {
	var sb strings.Builder
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || strings.HasSuffix(p, lock.FileName) {
			return nil
		}
		b, _ := os.ReadFile(p)
		sb.WriteString(p + ":" + string(bytes.TrimSpace(b)) + "\n")
		return nil
	})
	return sb.String()
}
