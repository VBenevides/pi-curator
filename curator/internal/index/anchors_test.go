package index

import (
	"encoding/json"
	"os"
	"path/filepath"
	"pi-curator/curator/internal/record"
	"testing"
)

func TestHybridPreservesExactAnchorsAndBehavioralVocabulary(t *testing.T) {
	store := t.TempDir()
	if err := os.Mkdir(filepath.Join(store, "journal"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store, "journal", "events.jsonl")
	appendEvent(t, path, "wrong", "foo/bar.ts changes sorting behavior")
	appendEvent(t, path, "right", "foo-bar.ts failed rename preserves original file unchanged")
	appendEvent(t, path, "other", "lib/store.go failed rename leaves the original file unchanged")
	ev := record.Event{Schema: record.SchemaEvent, ID: "call", SessionID: "s", Category: record.CategoryToolCall, ToolName: "bash", CallID: "c", CreatedAtUTC: "2026-10-03T00:00:01Z", PolicyVersion: record.PolicyVersion, Content: `{"command":"go test ./internal/index"}`}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
	f.Close()
	idx, err := Open(store)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if _, err = idx.Update(false); err != nil {
		t.Fatal(err)
	}
	res, err := idx.Hybrid("foo-bar.ts", 1)
	if err != nil || len(res.Sessions) != 1 || res.Sessions[0].Hits[0].EventID != "right" {
		t.Fatalf("exact punctuation: %v %v", res, err)
	}
	res, err = idx.Hybrid("failed rename original unchanged", 5)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, session := range res.Sessions {
		for _, hit := range session.Hits {
			found[hit.EventID] = true
		}
	}
	if !found["right"] || !found["other"] {
		t.Fatalf("lost original vocabulary: %v", res)
	}
	res, err = idx.Hybrid("go test ./internal/index", 5)
	if err != nil {
		t.Fatal(err)
	}
	found = map[string]bool{}
	for _, session := range res.Sessions {
		for _, hit := range session.Hits {
			found[hit.EventID] = true
		}
	}
	if !found["call"] {
		t.Fatalf("command anchor unavailable: %v", res)
	}
	res, err = idx.Hybrid("nonexistentword", 5)
	if err != nil || res.Matched != 0 {
		t.Fatalf("false match: %v %v", res, err)
	}
}
