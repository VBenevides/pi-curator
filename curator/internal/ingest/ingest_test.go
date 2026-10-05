package ingest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"pi-curator/curator/internal/journal"
	"pi-curator/curator/internal/lock"
	"pi-curator/curator/internal/onboard"
	"pi-curator/curator/internal/record"
)

var fixed = time.Date(2026, 9, 18, 14, 22, 1, 0, time.UTC)

func opts() Options {
	return Options{Now: func() time.Time { return fixed }, LockTimeout: 200 * time.Millisecond}
}

func readyRepo(t *testing.T) string {
	t.Helper()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if _, err := onboard.Initialize(root); err != nil {
		t.Fatal(err)
	}
	return root
}

func ev(id, cat, content string) EventIn {
	return EventIn{ID: id, SessionID: "s1", Category: cat, Content: content}
}

func journalIDs(t *testing.T, root string) []string {
	t.Helper()
	var ids []string
	rep, err := journal.New(filepath.Join(root, ".curator")).Scan(func(e journal.Entry) error {
		ids = append(ids, e.Rec.(*record.Event).ID)
		return nil
	})
	if err != nil || !rep.Healthy() {
		t.Fatalf("scan: %v %+v", err, rep)
	}
	return ids
}

func outcomes(r Response) string {
	var parts []string
	for _, x := range r.Results {
		s := x.ID + "=" + x.Outcome
		if x.Code != "" {
			s += "/" + x.Code
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

func TestEveryVisibleCategoryIsJournaledWithCuratorAssignedTime(t *testing.T) {
	root := readyRepo(t)
	cats := []string{record.CategoryUserMessage, record.CategoryAssistantMessage, record.CategoryToolCall,
		record.CategoryToolResult, record.CategorySessionBoundary, record.CategoryTaskBoundary}
	var req Request
	for i, c := range cats {
		req.Events = append(req.Events, ev("e"+string(rune('a'+i)), c, "payload "+c))
	}
	resp := Run(root, req, opts())
	for _, r := range resp.Results {
		if r.Outcome != OutcomeDurable {
			t.Fatalf("outcomes: %s", outcomes(resp))
		}
	}
	if got := journalIDs(t, root); len(got) != len(cats) {
		t.Fatalf("journaled %v", got)
	}
	data, _ := os.ReadFile(filepath.Join(root, ".curator", "journal", "events.jsonl"))
	if n := strings.Count(string(data), `"created_at_utc":"2026-09-18T14:22:01Z"`); n != len(cats) {
		t.Fatalf("created_at_utc stamped on %d records", n)
	}
}

func TestRepeatedCallbacksAreDuplicatesAndConflictsAreRejected(t *testing.T) {
	root := readyRepo(t)
	req := Request{Events: []EventIn{ev("e1", record.CategoryUserMessage, "hello")}}
	if r := Run(root, req, opts()); outcomes(r) != "e1=durable" {
		t.Fatal(outcomes(r))
	}
	if r := Run(root, req, opts()); outcomes(r) != "e1=duplicate" {
		t.Fatal(outcomes(r))
	}
	conflict := Request{Events: []EventIn{ev("e1", record.CategoryUserMessage, "different")}}
	if r := Run(root, conflict, opts()); outcomes(r) != "e1=rejected/conflicting_id" {
		t.Fatal(outcomes(r))
	}
	if got := journalIDs(t, root); len(got) != 1 {
		t.Fatalf("journal = %v", got)
	}
}

func TestDuplicatesWithinOneBatchWriteOneRecord(t *testing.T) {
	root := readyRepo(t)
	e := ev("e1", record.CategoryUserMessage, "same")
	r := Run(root, Request{Events: []EventIn{e, e, ev("e1", record.CategoryUserMessage, "other")}}, opts())
	if outcomes(r) != "e1=durable e1=duplicate e1=rejected/conflicting_id" {
		t.Fatal(outcomes(r))
	}
	if got := journalIDs(t, root); len(got) != 1 {
		t.Fatalf("journal = %v", got)
	}
}

func TestBadItemDoesNotBlockNeighbours(t *testing.T) {
	root := readyRepo(t)
	req := Request{Events: []EventIn{
		ev("ok1", record.CategoryUserMessage, "a"),
		ev("bad id", record.CategoryUserMessage, "b"),
		ev("huge", record.CategoryToolResult, strings.Repeat("x", MaxContentBytes+1)),
		{ID: "cat", SessionID: "s1", Category: "private_reasoning", Content: "thoughts"},
		ev("ok2", record.CategoryAssistantMessage, "c"),
	}}
	r := Run(root, req, opts())
	want := "ok1=durable bad id=rejected/invalid huge=rejected/event_too_large cat=rejected/invalid ok2=durable"
	if outcomes(r) != want {
		t.Fatalf("got %s", outcomes(r))
	}
	if got := journalIDs(t, root); strings.Join(got, ",") != "ok1,ok2" {
		t.Fatalf("journal = %v", got)
	}
}

func TestMechanicalNoiseIsStillCaptured(t *testing.T) {
	root := readyRepo(t)
	var req Request
	for _, id := range []string{"r1", "r2", "r3"} {
		req.Events = append(req.Events, ev(id, record.CategoryToolResult, "ok\n"))
	}
	Run(root, req, opts())
	if got := journalIDs(t, root); len(got) != 3 {
		t.Fatalf("low-value events dropped: %v", got)
	}
}

func TestSecretsNeverReachTheJournal(t *testing.T) {
	root := readyRepo(t)
	const key = "AKIAIOSFODNN7EXAMPLE"
	req := Request{Events: []EventIn{
		ev("m1", record.CategoryUserMessage, "use "+key),
		{ID: "t1", SessionID: "s1", Category: record.CategoryToolResult, Content: "A=1\nSECRET=zzzzzzzz", Paths: []string{"app/.env"}},
	}}
	r := Run(root, req, opts())
	if outcomes(r) != "m1=durable t1=durable" {
		t.Fatal(outcomes(r))
	}
	data, _ := os.ReadFile(filepath.Join(root, ".curator", "journal", "events.jsonl"))
	for _, leak := range []string{key, "zzzzzzzz", "A=1"} {
		if strings.Contains(string(data), leak) {
			t.Fatalf("journal leaked %q", leak)
		}
	}
}

func storedContent(t *testing.T, root, id string) string {
	t.Helper()
	var got string
	_, err := journal.New(filepath.Join(root, ".curator")).Scan(func(e journal.Entry) error {
		if r := e.Rec.(*record.Event); r.ID == id {
			got = r.Content
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestLongToolResultsAreCutExplicitlyAndOnlyAfterRedaction(t *testing.T) {
	root := readyRepo(t)
	const key = "AKIAIOSFODNN7EXAMPLE"
	// The key straddles the cut: a cut before redaction would leave a prefix.
	straddle := strings.Repeat("x", MaxToolResultBytes-11) + " " + key + " " + strings.Repeat("y", 500)
	multibyte := strings.Repeat("é", MaxToolResultBytes) // 2 bytes per rune, cut falls mid-rune for odd caps
	short := strings.Repeat("z", MaxToolResultBytes)
	r := Run(root, Request{Events: []EventIn{
		ev("r1", record.CategoryToolResult, straddle),
		ev("r2", record.CategoryToolResult, "a"+multibyte),
		ev("r3", record.CategoryToolResult, short),
		ev("m1", record.CategoryUserMessage, strings.Repeat("u", MaxToolResultBytes*2)),
	}}, opts())
	if outcomes(r) != "r1=durable r2=durable r3=durable m1=durable" {
		t.Fatal(outcomes(r))
	}
	if got := storedContent(t, root, "r1"); strings.Contains(got, "AKIA") || !strings.Contains(got, "[truncated: kept ") {
		t.Fatalf("r1 leaked a key prefix or lacks a marker: %q", got[len(got)-80:])
	}
	got := storedContent(t, root, "r2")
	body, marker, _ := strings.Cut(got, "\n[truncated: ")
	if !utf8.ValidString(got) || len(body) > MaxToolResultBytes || marker != "kept "+strconv.Itoa(len(body))+" of "+strconv.Itoa(len("a"+multibyte))+" bytes]" {
		t.Fatalf("r2 body %d bytes, marker %q, valid=%v", len(body), marker, utf8.ValidString(got))
	}
	if storedContent(t, root, "r3") != short {
		t.Fatal("a result at the limit must be stored whole")
	}
	if len(storedContent(t, root, "m1")) != MaxToolResultBytes*2 {
		t.Fatal("only tool results are cut")
	}
}

func TestCallerSuppliedCreatedAtIsKeptAndValidated(t *testing.T) {
	root := readyRepo(t)
	old, bad, fresh := ev("o1", record.CategoryUserMessage, "old"), ev("b1", record.CategoryUserMessage, "bad"), ev("f1", record.CategoryUserMessage, "fresh")
	old.CreatedAt = "2026-07-31T09:00:00Z"
	bad.CreatedAt = "2026-07-31 09:00"
	r := Run(root, Request{Events: []EventIn{old, bad, fresh}}, opts())
	if outcomes(r) != "o1=durable b1=rejected/invalid f1=durable" {
		t.Fatal(outcomes(r))
	}
	stamps := map[string]string{}
	if _, err := journal.New(filepath.Join(root, ".curator")).Scan(func(e journal.Entry) error {
		r := e.Rec.(*record.Event)
		stamps[r.ID] = r.CreatedAtUTC
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if stamps["o1"] != "2026-07-31T09:00:00Z" || stamps["f1"] == "" || stamps["f1"] == stamps["o1"] {
		t.Fatalf("stamps %v", stamps)
	}
	// The time is not part of the payload, so a retry without it is a duplicate, not a conflict.
	old.CreatedAt = ""
	if got := outcomes(Run(root, Request{Events: []EventIn{old}}, opts())); got != "o1=duplicate" {
		t.Fatal(got)
	}
}

func TestMemoryInspectionIsNotLearned(t *testing.T) {
	root := readyRepo(t)
	req := Request{Events: []EventIn{
		ev("c1", record.CategoryToolCall, `{"command":"rg -n token .curator/memory/active.jsonl"}`),
		ev("c2", record.CategoryToolCall, `{"command":"curator show ep_1"}`),
		{ID: "c3", SessionID: "s1", Category: record.CategoryToolResult, Paths: []string{"/repo/.curator/memory/active.jsonl"}},
		ev("c4", record.CategoryToolCall, `{"command":"rg token src"}`),
		ev("c5", record.CategoryUserMessage, "please look at .curator/memory/active.jsonl"),
	}}
	r := Run(root, req, opts())
	want := "c1=rejected/memory_inspection c2=rejected/memory_inspection c3=rejected/memory_inspection c4=durable c5=durable"
	if outcomes(r) != want {
		t.Fatalf("got %s", outcomes(r))
	}
}

func TestNotReadyRepositoryCreatesNothing(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	r := Run(root, Request{Events: []EventIn{ev("e1", record.CategoryUserMessage, "x")}}, opts())
	if outcomes(r) != "e1=rejected/not_ready" {
		t.Fatal(outcomes(r))
	}
	if entries, _ := os.ReadDir(root); len(entries) != 1 { // only .git
		t.Fatalf("files created: %v", entries)
	}
	notGit := t.TempDir()
	r = Run(notGit, Request{Events: []EventIn{ev("e1", record.CategoryUserMessage, "x")}}, opts())
	if outcomes(r) != "e1=rejected/not_ready" {
		t.Fatal(outcomes(r))
	}
}

func TestLockTimeoutIsFailedNotDurableAndChangesNothing(t *testing.T) {
	root := readyRepo(t)
	h, err := lock.Acquire(filepath.Join(root, ".curator", lock.FileName), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	r := Run(root, Request{Events: []EventIn{ev("e1", record.CategoryUserMessage, "x")}}, opts())
	_ = h.Release()
	if outcomes(r) != "e1=failed/lock_timeout" {
		t.Fatal(outcomes(r))
	}
	if got := journalIDs(t, root); len(got) != 0 {
		t.Fatalf("journal written without lock: %v", got)
	}
	if r := Run(root, Request{Events: []EventIn{ev("e1", record.CategoryUserMessage, "x")}}, opts()); outcomes(r) != "e1=durable" {
		t.Fatalf("retry after timeout: %s", outcomes(r))
	}
}

func TestWriteFailureIsFailedAndRetrySucceeds(t *testing.T) {
	root := readyRepo(t)
	Run(root, Request{Events: []EventIn{ev("e0", record.CategoryUserMessage, "x")}}, opts())
	jdir := filepath.Join(root, ".curator", "journal")
	jfile := filepath.Join(jdir, "events.jsonl")
	if err := os.Chmod(jfile, 0o400); err != nil {
		t.Fatal(err)
	}
	r := Run(root, Request{Events: []EventIn{ev("e1", record.CategoryUserMessage, "y")}}, opts())
	if outcomes(r) != "e1=failed/write_failed" {
		t.Fatal(outcomes(r))
	}
	os.Chmod(jfile, 0o600)
	if got := journalIDs(t, root); strings.Join(got, ",") != "e0" {
		t.Fatalf("journal = %v", got)
	}
	if r := Run(root, Request{Events: []EventIn{ev("e1", record.CategoryUserMessage, "y")}}, opts()); outcomes(r) != "e1=durable" {
		t.Fatal(outcomes(r))
	}
}

func TestIncompleteTailFailsIngestionButKeepsCompletedRecords(t *testing.T) {
	root := readyRepo(t)
	Run(root, Request{Events: []EventIn{ev("e0", record.CategoryUserMessage, "x")}}, opts())
	jfile := filepath.Join(root, ".curator", "journal", "events.jsonl")
	f, _ := os.OpenFile(jfile, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"schema":"cura`)
	f.Close()
	r := Run(root, Request{Events: []EventIn{ev("e1", record.CategoryUserMessage, "y")}}, opts())
	if outcomes(r) != "e1=failed/journal_incomplete_tail" || len(r.JournalWarnings) == 0 {
		t.Fatalf("%s %v", outcomes(r), r.JournalWarnings)
	}
}

func TestCallerCannotSetCreationTimeOrUnknownFields(t *testing.T) {
	for _, body := range []string{
		`{"events":[{"id":"e1","session_id":"s","category":"user_message","created_at_utc":"2001-01-01T00:00:00Z"}]}`,
		`{"events":[{"id":"e1","session_id":"s","category":"user_message","recall_count":3}]}`,
		`{"events":[]}`,
		`{"events":[{"id":"e1"}]} {}`,
	} {
		if _, err := ParseRequest([]byte(body)); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
	if _, err := ParseRequest([]byte(`{"request_id":"r1","events":[{"id":"e1","session_id":"s","category":"user_message","content":"x"}]}`)); err != nil {
		t.Fatal(err)
	}
}

func TestReplayCannotChangeObservedToolStatus(t *testing.T) {
	root := readyRepo(t)
	failed, succeeded := true, false
	result := ev("result", "tool_result", "printed PASS")
	result.CallID, result.ToolName, result.IsError = "call", "bash", &succeeded
	request := Request{Events: []EventIn{result}}
	if got := Run(root, request, opts()); got.Results[0].Outcome != OutcomeDurable {
		t.Fatal(outcomes(got))
	}
	if got := Run(root, request, opts()); got.Results[0].Outcome != OutcomeDuplicate {
		t.Fatal(outcomes(got))
	}
	request.Events[0].IsError = &failed
	if got := Run(root, request, opts()); got.Results[0].Outcome != OutcomeRejected || got.Results[0].Code != CodeConflictingID {
		t.Fatal(outcomes(got))
	}
	if ids := journalIDs(t, root); len(ids) != 1 {
		t.Fatalf("replay appended conflicting observations: %v", ids)
	}
}
