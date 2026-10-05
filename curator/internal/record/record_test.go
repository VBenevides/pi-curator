package record

import (
	"bytes"
	"strings"
	"testing"
)

const ts = "2026-09-18T14:22:01Z"

func validRecords() map[Kind]interface{ Validate() error } {
	return map[Kind]interface{ Validate() error }{
		"event": Event{Schema: SchemaEvent, ID: "e1", CreatedAtUTC: ts, SessionID: "s1",
			Category: CategoryUserMessage, Content: "hi", PolicyVersion: PolicyVersion},
		"episode": Episode{Schema: SchemaEpisode, ID: "ep_1", CreatedAtUTC: ts, SessionID: "s1",
			EventIDs: []string{"e1"}, ExtractionVersion: ExtractionVersion},
		"lifecycle": Lifecycle{Schema: SchemaLifecycle, ID: "l1", CreatedAtUTC: ts, Action: ActionPin, Target: "ep_1"},
		"progress":  Progress{Schema: SchemaProgress, ID: "p1", CreatedAtUTC: ts, Stage: "episodes", Cursor: "e1", Version: ExtractionVersion},
		"model_decision": ModelDecision{Schema: SchemaModelDecision, ID: "d1", CreatedAtUTC: ts, EpisodeID: "ep_1",
			ModelVersion: "m1", PolicyVersion: PolicyVersion, Scores: map[string]float64{"retention": 0.5}, Action: "keep"},
		"feedback": Feedback{Schema: SchemaFeedback, ID: "f1", CreatedAtUTC: ts, EpisodeID: "ep_1", Label: "useful", Source: "user"},
		"memory": Memory{Schema: SchemaMemory, ID: "ep_1", CreatedAtUTC: ts, Status: "active", Session: "s1",
			ExtractionVersion: ExtractionVersion, SearchText: "hi", SourceRefs: []string{"event:e1"}},
	}
}

func TestEveryRecordTypeRoundTripsAndPreservesCreationTime(t *testing.T) {
	for kind, rec := range validRecords() {
		line, err := Encode(rec)
		if err != nil {
			t.Fatalf("%s: encode: %v", kind, err)
		}
		if bytes.ContainsAny(line, "\n") {
			t.Fatalf("%s: record spans lines", kind)
		}
		if !strings.Contains(string(line), `"created_at_utc":"`+ts+`"`) {
			t.Fatalf("%s: created_at_utc missing from %s", kind, line)
		}
		gotKind, _, err := Decode(line)
		if err != nil || gotKind != kind {
			t.Fatalf("%s: decode = %q, %v", kind, gotKind, err)
		}
	}
}

func TestInvalidTimestampsRejectedForEveryType(t *testing.T) {
	bad := []string{"", "2026-09-18T14:22:01", "2026-09-18T14:22:01+00:00", "2026-09-18T14:22:01+02:00", "yesterday", "2026-13-18T14:22:01Z"}
	for kind, rec := range validRecords() {
		for _, b := range bad {
			var rec2 interface{ Validate() error }
			switch r := rec.(type) {
			case Event:
				r.CreatedAtUTC = b
				rec2 = r
			case Episode:
				r.CreatedAtUTC = b
				rec2 = r
			case Lifecycle:
				r.CreatedAtUTC = b
				rec2 = r
			case Progress:
				r.CreatedAtUTC = b
				rec2 = r
			case ModelDecision:
				r.CreatedAtUTC = b
				rec2 = r
			case Feedback:
				r.CreatedAtUTC = b
				rec2 = r
			case Memory:
				r.CreatedAtUTC = b
				rec2 = r
			}
			if err := rec2.Validate(); err == nil {
				t.Errorf("%s accepted created_at_utc %q", kind, b)
			}
		}
	}
}

func TestDecodeRejectsUnknownFieldsSchemasAndTrailingData(t *testing.T) {
	good, _ := Encode(validRecords()["lifecycle"])
	cases := map[string]string{
		"unknown field":  strings.Replace(string(good), `"action"`, `"recall_count":1,"action"`, 1),
		"unknown schema": strings.Replace(string(good), SchemaLifecycle, "curator.lifecycle.v9", 1),
		"trailing":       string(good) + `{}`,
		"not json":       `{"schema":`,
	}
	for name, line := range cases {
		if _, _, err := Decode([]byte(line)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSupersedeNeedsDistinctSuccessor(t *testing.T) {
	l := Lifecycle{Schema: SchemaLifecycle, ID: "l1", CreatedAtUTC: ts, Action: ActionSupersede, Target: "a"}
	if l.Validate() == nil {
		t.Fatal("supersede without successor accepted")
	}
	l.SupersededBy = "a"
	if l.Validate() == nil {
		t.Fatal("self-supersede accepted")
	}
	l.SupersededBy = "b"
	if err := l.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestReplayIsDeterministicAndIdempotent(t *testing.T) {
	mk := func(id, action, target, by string) Lifecycle {
		return Lifecycle{Schema: SchemaLifecycle, ID: id, CreatedAtUTC: ts, Action: action, Target: target, SupersededBy: by}
	}
	log := []Lifecycle{
		mk("l1", ActionPin, "a", ""), mk("l2", ActionArchive, "b", ""), mk("l3", ActionSupersede, "c", "a"),
		mk("l4", ActionUnpin, "a", ""), mk("l5", ActionRestore, "b", ""), mk("l6", ActionPin, "a", ""),
	}
	want := Replay(log)
	// A retried duplicate of an earlier record must not undo later state.
	dup := append(append([]Lifecycle{}, log...), log[0], log[1])
	got := Replay(dup)
	for _, k := range []string{"a", "b", "c"} {
		if got[k] != want[k] {
			t.Errorf("%s: state %+v != %+v after duplicates", k, got[k], want[k])
		}
	}
	if !want["a"].Pinned || want["b"].Archived || want["c"].SupersededBy != "a" {
		t.Fatalf("unexpected replay state: %+v", want)
	}
}

func TestMemoryRefsResolveAgainstAuthoritativeRecords(t *testing.T) {
	m := validRecords()["memory"].(Memory)
	known := map[string]bool{"event:e1": true}
	exists := func(kind, id string) bool { return known[kind+":"+id] }
	if err := m.ResolveRefs(exists); err != nil {
		t.Fatal(err)
	}
	m.SourceRefs = []string{"event:e1", "event:ghost"}
	if err := m.ResolveRefs(exists); err == nil {
		t.Fatal("invented source ref accepted")
	}
	m.SourceRefs = []string{"file:/etc/passwd"}
	if m.Validate() == nil {
		t.Fatal("malformed ref accepted")
	}
}

func TestMemorySearchTextMustBeSingleLine(t *testing.T) {
	m := validRecords()["memory"].(Memory)
	m.SearchText = "a\nb"
	if m.Validate() == nil {
		t.Fatal("multi-line search_text accepted")
	}
}
