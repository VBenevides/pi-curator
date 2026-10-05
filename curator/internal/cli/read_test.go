package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func readFixture(t *testing.T, events []map[string]any, gaps []map[string]any) string {
	t.Helper()
	root := gitInit(t)
	if code := run([]string{"init", "--consent", "--cwd", root}, io.Discard, io.Discard); code != 0 {
		t.Fatal("init failed")
	}
	body, err := json.Marshal(map[string]any{"events": events, "gaps": gaps})
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := Run([]string{"ingest", "--cwd", root}, bytes.NewReader(body), &out, &errb, "t"); code != 0 {
		t.Fatalf("ingest: %s %s", out.String(), errb.String())
	}
	var response struct {
		Results []struct {
			Outcome string `json:"outcome"`
		} `json:"results"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	for _, result := range response.Results {
		if result.Outcome != "durable" {
			t.Fatalf("ingest did not persist fixture: %s", out.String())
		}
	}
	return root
}
func evidenceEvent(id, category, content string) map[string]any {
	return map[string]any{"id": id, "session_id": "s", "category": category, "content": content}
}

func TestStoredEvidencePaginationAndUnknownID(t *testing.T) {
	text := strings.Repeat("雪😀<&\"\\\n", 3000)
	root := readFixture(t, []map[string]any{evidenceEvent("e1", "user_message", text)}, nil)
	for _, indexed := range []bool{false, true} {
		if indexed {
			if code := run([]string{"index", "--cwd", root}, io.Discard, io.Discard); code != 0 {
				t.Fatal("index")
			}
		}
		var reconstructed strings.Builder
		cursor, count := 0, 0
		for {
			var out, errb bytes.Buffer
			args := []string{"read", "--cwd", root, "--budget", "256", "--cursor", strconv.Itoa(cursor)}
			if indexed {
				args = append(args, "--indexed")
			}
			args = append(args, "e1")
			if code := run(args, &out, &errb); code != 0 {
				t.Fatal(errb.String())
			}
			if out.Len() > 1024 || !bytes.HasSuffix(out.Bytes(), []byte{'\n'}) {
				t.Fatalf("serialized budget/newline: %d", out.Len())
			}
			var p evidencePage
			if err := json.Unmarshal(out.Bytes(), &p); err != nil {
				t.Fatal(err)
			}
			if p.Cursor != cursor || p.StoredBytes != len(text) || p.Redactions != 0 {
				t.Fatalf("metadata: %+v", p)
			}
			reconstructed.WriteString(p.Content)
			count++
			if p.Complete {
				break
			}
			if p.Next <= cursor {
				t.Fatal("no progress")
			}
			cursor = p.Next
		}
		if count < 2 || reconstructed.String() != text {
			t.Fatal("lost or duplicated stored evidence")
		}
	}
	for _, args := range [][]string{{"missing"}, {"--cursor", "1", "e1"}, {"--cursor", "999999", "e1"}} {
		var out, errb bytes.Buffer
		if code := run(append([]string{"read", "--cwd", root}, args...), &out, &errb); code == 0 || out.Len() != 0 || errb.Len() == 0 {
			t.Fatal("invalid ID/cursor must fail observably")
		}
	}
}

func TestEvidenceBatchLinksAndFidelity(t *testing.T) {
	call := evidenceEvent("call", "tool_call", "run command")
	call["call_id"] = "c"
	result := evidenceEvent("result", "tool_result", strings.Repeat("雪", 2000)+" API_KEY=supersecretvalue1")
	result["call_id"] = "c"
	result["is_error"] = true
	orphan := evidenceEvent("orphan", "tool_result", "no call captured")
	orphan["call_id"] = "missing"
	root := readFixture(t, []map[string]any{call, result, orphan}, []map[string]any{{"session_id": "s", "event_id": "lost"}})
	var out, errb bytes.Buffer
	if code := run([]string{"read", "--cwd", root, "--budget", "800", "call", "result", "orphan", "call"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	if out.Len() > 3200 {
		t.Fatal("batch budget exceeded")
	}
	var pages []evidencePage
	if err := json.Unmarshal(out.Bytes(), &pages); err != nil {
		t.Fatal(err)
	}
	if len(pages) != 3 || len(pages[0].Linked) != 1 || pages[0].Linked[0] != "result" || pages[0].MissingPartner || pages[1].IsError == nil || !*pages[1].IsError || pages[1].Redactions != 1 || !strings.Contains(pages[1].CaptureFidelity, "2048") || !pages[2].MissingPartner || pages[0].CaptureGaps != 1 {
		t.Fatalf("incorrect evidence metadata: %+v", pages)
	}
	if code := run([]string{"read", "--cwd", root, "--budget", "256", "call", "result", "orphan"}, io.Discard, &errb); code == 0 {
		t.Fatal("insufficient envelope budget succeeded")
	}
}

func TestEvidenceRejectsUnhealthyAndTamperedSources(t *testing.T) {
	root := readFixture(t, []map[string]any{evidenceEvent("e1", "user_message", "original")}, nil)
	if code := run([]string{"index", "--cwd", root}, io.Discard, io.Discard); code != 0 {
		t.Fatal("index")
	}
	path := filepath.Join(root, ".curator", "journal", "events.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(data, []byte("original"), []byte("tampered"), 1)
	if err := os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"read", "--cwd", root, "--indexed", "e1"}, io.Discard, io.Discard); code == 0 {
		t.Fatal("tampered index source succeeded")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{broken}\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	var out, errb bytes.Buffer
	if code := run([]string{"read", "--cwd", root, "e1"}, &out, &errb); code == 0 || out.Len() != 0 || !strings.Contains(errb.String(), "unhealthy") {
		t.Fatal("partial journal accepted")
	}
}
