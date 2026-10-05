package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestEvidenceLinkedMetadataLimitAndExactPartnerRead(t *testing.T) {
	call := evidenceEvent("call", "tool_call", "exact command")
	call["call_id"] = "c"
	result := evidenceEvent("result", "tool_result", "exact output")
	result["call_id"] = "c"
	root := readFixture(t, []map[string]any{call, result}, nil)
	for _, indexed := range []bool{false, true} {
		args := []string{"read", "--cwd", root}
		if indexed {
			if code := run([]string{"index", "--cwd", root}, io.Discard, io.Discard); code != 0 {
				t.Fatal("index")
			}
			args = append(args, "--indexed")
		}
		var out, errb bytes.Buffer
		if code := run(append(args, "call"), &out, &errb); code != 0 {
			t.Fatal(errb.String())
		}
		var p evidencePage
		if err := json.Unmarshal(out.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		if len(p.Linked) != 1 || p.Linked[0] != "result" || !p.LinksComplete {
			t.Fatalf("links: %+v", p)
		}
		out.Reset()
		if code := run(append(args, p.Linked[0]), &out, &errb); code != 0 {
			t.Fatal(errb.String())
		}
		if err := json.Unmarshal(out.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		if p.Content != "exact output" || !p.Complete {
			t.Fatal("linked read was not exact")
		}
	}
	events := []map[string]any{call}
	for i := range 101 {
		ev := evidenceEvent(fmt.Sprintf("link%d", i), "tool_result", "output")
		ev["call_id"] = "c"
		events = append(events, ev)
	}
	crowded := readFixture(t, events, nil)
	var out, errb bytes.Buffer
	if code := run([]string{"read", "--cwd", crowded, "call"}, &out, &errb); code == 0 || out.Len() != 0 || !strings.Contains(errb.String(), "links exceed 100") {
		t.Fatal("link overflow hidden")
	}
}

func TestCapturedToolResultPagesEqualStoredSourceNotOriginal(t *testing.T) {
	original := strings.Repeat("雪😀", 2000)
	root := readFixture(t, []map[string]any{evidenceEvent("result", "tool_result", original)}, nil)
	raw, err := os.ReadFile(filepath.Join(root, ".curator", "journal", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Content == original || !strings.Contains(stored.Content, "[truncated: kept") {
		t.Fatal("fixture was not capture-limited")
	}
	var reconstructed strings.Builder
	for cursor := 0; ; {
		var out, errb bytes.Buffer
		if code := run([]string{"read", "--cwd", root, "--budget", "256", "--cursor", strconv.Itoa(cursor), "result"}, &out, &errb); code != 0 {
			t.Fatal(errb.String())
		}
		var p evidencePage
		if err := json.Unmarshal(out.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(p.CaptureFidelity, "omitted original text unavailable") {
			t.Fatal("capture limitation hidden")
		}
		reconstructed.WriteString(p.Content)
		if p.Complete {
			break
		}
		if p.Next <= cursor {
			t.Fatal("no progress")
		}
		cursor = p.Next
	}
	if reconstructed.String() != stored.Content {
		t.Fatal("stored capture was not reconstructed exactly")
	}
}
