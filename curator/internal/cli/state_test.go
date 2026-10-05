package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"pi-curator/curator/internal/record"
	"pi-curator/curator/internal/search"
)

func TestScopedStateRequiresExplicitReplacementAndKeepsHistory(t *testing.T) {
	root := gitInit(t)
	invoke := func(args ...string) []byte {
		var out, errb bytes.Buffer
		args = append([]string{args[0], "--cwd", root}, args[1:]...)
		if code := run(args, &out, &errb); code != 0 {
			t.Fatalf("%v: %s", args, errb.String())
		}
		return out.Bytes()
	}
	invoke("init", "--consent")
	var out, errb bytes.Buffer
	body := `{"events":[{"id":"old","session_id":"s","category":"user_message","content":"setting uses per-user configuration"},{"id":"new","session_id":"s","category":"user_message","content":"setting uses repository configuration","created_at":"2030-01-01T00:00:00Z"},{"id":"other","session_id":"s","category":"user_message","content":"setting uses feature configuration"}]}`
	if code := Run([]string{"ingest", "--cwd", root}, strings.NewReader(body), &out, &errb, "test"); code != 0 {
		t.Fatal(errb.String())
	}
	declare := func(source, branch string) record.Memory {
		args := []string{"state", "--key", "setting", "--source", source}
		if branch != "" {
			args = append(args, "--branch", branch)
		}
		var fact record.Memory
		if err := json.Unmarshal(invoke(args...), &fact); err != nil {
			t.Fatal(err)
		}
		return fact
	}
	old := declare("old", "")
	newer := declare("new", "")
	other := declare("other", "other-branch")
	again := declare("old", "")
	if again.ID != old.ID || again.CreatedAtUTC != old.CreatedAtUTC {
		t.Fatal("registration was not idempotent")
	}
	query := func(history bool) map[string]bool {
		args := []string{"memory-search", "--engine", "state", "--query", "setting", "--format", "json"}
		if history {
			args = append(args, "--history")
		}
		var result search.Result
		if err := json.Unmarshal(invoke(args...), &result); err != nil {
			t.Fatal(err)
		}
		ids := map[string]bool{}
		for _, session := range result.Sessions {
			for _, hit := range session.Hits {
				ids[hit.EventID] = true
			}
		}
		return ids
	}
	before := query(false)
	if !before["old"] || !before["new"] || before["other"] {
		t.Fatalf("implicit replacement or branch leak: %v", before)
	}
	errb.Reset()
	if code := run([]string{"supersede", "--cwd", root, "--by", other.ID, old.ID}, &out, &errb); code != 1 {
		t.Fatal("cross-branch supersession accepted")
	}
	invoke("supersede", "--by", newer.ID, old.ID)
	after := query(false)
	if after["old"] || !after["new"] || after["other"] {
		t.Fatalf("wrong current state: %v", after)
	}
	historical := query(true)
	if !historical["old"] || !historical["new"] {
		t.Fatalf("history lost: %v", historical)
	}
	var detail struct {
		Events []record.Event `json:"events"`
	}
	if err := json.Unmarshal(invoke("show", old.ID), &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Events) != 1 || detail.Events[0].Content != "setting uses per-user configuration" {
		t.Fatalf("historical source changed: %+v", detail)
	}
}
