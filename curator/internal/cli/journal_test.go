package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecoverUnblocksIngestionAfterTornWrite(t *testing.T) {
	root := gitInit(t)
	var out, errb bytes.Buffer
	run([]string{"init", "--consent", "--cwd", root}, io.Discard, &errb)
	ev := func(id string) string {
		return `{"events":[{"id":"` + id + `","session_id":"s","category":"user_message","content":"x"}]}`
	}
	ingest := func(body string) int {
		out.Reset()
		return Run([]string{"ingest", "--cwd", root}, strings.NewReader(body), &out, &errb, "t")
	}
	if ingest(ev("e1")) != 0 {
		t.Fatal("first ingest failed")
	}
	jp := filepath.Join(root, ".curator", "journal", "events.jsonl")
	f, _ := os.OpenFile(jp, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"schema":"curator.ev`)
	f.Close()
	if ingest(ev("e2")) != 1 || !strings.Contains(out.String(), "journal_incomplete_tail") {
		t.Fatalf("torn tail not reported: %s", out.String())
	}
	out.Reset()
	if code := run([]string{"recover", "--cwd", root}, &out, &errb); code != 0 || !strings.Contains(out.String(), `"recovered":true`) {
		t.Fatalf("recover: %d %s", code, out.String())
	}
	if ingest(ev("e2")) != 0 {
		t.Fatalf("ingest after recover: %s", out.String())
	}
	if ingest(`{"gaps":[{"session_id":"s","event_id":"e9"}]}`) != 0 {
		t.Fatalf("gap report: %s", out.String())
	}
	out.Reset()
	if code := run([]string{"gaps", "--cwd", root}, &out, &errb); code != 0 || !strings.Contains(out.String(), `"event_id":"e9"`) || !strings.Contains(out.String(), `"recovered":false`) {
		t.Fatalf("gaps: %d %s", code, out.String())
	}
}
