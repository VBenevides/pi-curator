package index

import (
	"encoding/json"
	"os"
	"path/filepath"
	"pi-curator/curator/internal/record"
	"strings"
	"testing"
)

func appendEvent(t *testing.T, path, id, content string) {
	t.Helper()
	ev := record.Event{Schema: record.SchemaEvent, ID: id, SessionID: "s", Category: record.CategoryUserMessage, CreatedAtUTC: "2026-10-03T00:00:00Z", PolicyVersion: record.PolicyVersion, Content: content}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestIncrementalIndexResumeRebuildAndEvidence(t *testing.T) {
	store := t.TempDir()
	if err := os.Mkdir(filepath.Join(store, "journal"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store, "journal", "events.jsonl")
	appendEvent(t, path, "e1", "atomic 雪 writes preserve old data")
	idx, err := Open(store)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	p, err := idx.Update(false)
	if err != nil || p.Added != 1 || p.PendingBytes != 0 {
		t.Fatalf("initial %v %v", p, err)
	}
	p, err = idx.Update(false)
	if err != nil || p.Added != 0 {
		t.Fatalf("repeat %v %v", p, err)
	}
	ev, err := idx.Read("e1")
	if err != nil || ev.Content != "atomic 雪 writes preserve old data" {
		t.Fatalf("evidence %v %v", ev, err)
	}
	appendEvent(t, path, "e2", "case insensitive ordering")
	if _, err = idx.Read("e1"); err == nil {
		t.Fatal("stale index accepted")
	}
	p, err = idx.Update(false)
	if err != nil || p.Added != 1 {
		t.Fatalf("resume %v %v", p, err)
	}
	res, err := idx.Search("atomic", 5)
	if err != nil || len(res.Sessions) != 1 || res.Sessions[0].Hits[0].EventID != "e1" {
		t.Fatalf("search %v %v", res, err)
	}
	if _, err = idx.Read("missing"); err == nil {
		t.Fatal("missing ID accepted")
	}
	if _, err = idx.db.Exec("UPDATE checkpoint SET version=0"); err != nil {
		t.Fatal(err)
	}
	p, err = idx.Update(false)
	if err != nil || !p.Rebuilt || p.Added != 2 {
		t.Fatalf("schema rebuild %v %v", p, err)
	}
	if err = os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	appendEvent(t, path, "e3", "replacement history")
	p, err = idx.Update(false)
	if err != nil || !p.Rebuilt || p.Added != 1 {
		t.Fatalf("replacement %v %v", p, err)
	}
	if _, err = idx.Read("e1"); err == nil {
		t.Fatal("old evidence retained after replacement")
	}
}

func TestIncompleteTailAndMalformedRecordRemainObservable(t *testing.T) {
	store := t.TempDir()
	os.Mkdir(filepath.Join(store, "journal"), 0700)
	path := filepath.Join(store, "journal", "events.jsonl")
	appendEvent(t, path, "e1", "known useful decision")
	idx, err := Open(store)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("{partial")
	f.Close()
	p, err := idx.Update(false)
	if err != nil || p.Added != 1 || p.PendingBytes != 8 {
		t.Fatalf("partial %v %v", p, err)
	}
	if _, err = idx.Search("known", 5); err == nil {
		t.Fatal("incomplete journal presented as current")
	}
	if err = os.Truncate(path, p.Offset); err != nil {
		t.Fatal(err)
	}
	if _, err = idx.Update(false); err != nil {
		t.Fatal(err)
	}
	if _, err = idx.Read("e1"); err != nil {
		t.Fatal(err)
	}
	f, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("not json\n")
	f.Close()
	if _, err = idx.Update(false); err == nil || !strings.Contains(err.Error(), "offset") {
		t.Fatalf("malformed: %v", err)
	}
}

func TestReadDetectsChangedStoredBytesAndSymlinks(t *testing.T) {
	store := t.TempDir()
	os.Mkdir(filepath.Join(store, "journal"), 0700)
	path := filepath.Join(store, "journal", "events.jsonl")
	appendEvent(t, path, "e1", strings.Repeat("a", 600))
	appendEvent(t, path, "e2", strings.Repeat("b", 600))
	idx, err := Open(store)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if _, err = idx.Update(false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), strings.Repeat("a", 600), strings.Repeat("c", 600), 1))
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = idx.Read("e1"); err == nil {
		t.Fatal("changed evidence accepted")
	}
	other := t.TempDir()
	if err = os.Symlink(filepath.Join(store, "journal"), filepath.Join(other, "journal")); err != nil {
		t.Fatal(err)
	}
	if _, err = Open(other); err == nil {
		t.Fatal("journal symlink accepted")
	}
}
