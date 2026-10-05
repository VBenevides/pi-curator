package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLifecycleCommandsEndToEnd(t *testing.T) {
	root := gitInit(t)
	var out, errb bytes.Buffer
	if code := run([]string{"init", "--consent", "--cwd", root}, &out, &errb); code != 0 {
		t.Fatalf("init: %d", code)
	}
	body := `{"events":[{"id":"e1","session_id":"s","category":"user_message","content":"rotate keys"},{"id":"e2","session_id":"s","category":"task_boundary"}]}`
	Run([]string{"ingest", "--cwd", root}, strings.NewReader(body), &out, &errb, "t")
	out.Reset()
	if code := run([]string{"maintain", "--dry-run", "--cwd", root}, &out, &errb); code != 0 || !strings.Contains(out.String(), `"new_episodes":1`) {
		t.Fatalf("dry-run: %d %s", code, out.String())
	}
	if _, err := os.Stat(filepath.Join(root, ".curator", "memory")); err == nil {
		t.Fatal("dry-run wrote views")
	}
	run([]string{"maintain", "--cwd", root}, io.Discard, &errb)
	out.Reset()
	if code := run([]string{"list", "--cwd", root}, &out, &errb); code != 0 {
		t.Fatalf("list: %d %s", code, errb.String())
	}
	var entry struct{ Memory struct{ ID string } }
	if err := json.Unmarshal(out.Bytes(), &entry); err != nil || entry.Memory.ID == "" {
		t.Fatalf("list output %q: %v", out.String(), err)
	}
	id := entry.Memory.ID
	if code := run([]string{"archive", "--cwd", root, id}, io.Discard, &errb); code != 0 {
		t.Fatalf("archive: %d %s", code, errb.String())
	}
	out.Reset()
	run([]string{"list", "--cwd", root}, &out, &errb)
	if out.Len() != 0 {
		t.Fatalf("archived memory still listed active: %s", out.String())
	}
	if code := run([]string{"restore", "--cwd", root, id}, io.Discard, &errb); code != 0 {
		t.Fatalf("restore: %d", code)
	}
	out.Reset()
	if code := run([]string{"show", "--cwd", root, id}, &out, &errb); code != 0 || !strings.Contains(out.String(), "rotate keys") {
		t.Fatalf("show: %d %s", code, out.String())
	}
	if code := run([]string{"supersede", "--cwd", root, id}, io.Discard, io.Discard); code != 2 {
		t.Fatalf("supersede without --by: %d", code)
	}
	if code := run([]string{"pin", "--cwd", root}, io.Discard, io.Discard); code != 2 {
		t.Fatalf("pin without id: %d", code)
	}
	if code := run([]string{"pin", "--cwd", root, "mem_unknown"}, io.Discard, io.Discard); code != 1 {
		t.Fatalf("pin unknown: %d", code)
	}
}
