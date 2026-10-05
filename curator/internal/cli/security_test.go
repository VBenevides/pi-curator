package cli

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSecretsAbsentFromEveryPersistedAndPrintedSurface seeds synthetic
// secrets and denied content, drives the real command surface, then scans the
// whole store and every captured output for leaks.
func TestSecretsAbsentFromEveryPersistedAndPrintedSurface(t *testing.T) {
	const (
		awsKey   = "AKIAIOSFODNN7EXAMPLE"
		envValue = "hunter2hunter2"
		pemBody  = "MIIEvQIBADANBgkqhkiG9w0BAQEFAASC"
	)
	root := gitInit(t)
	var captured bytes.Buffer // everything curator printed
	var errb bytes.Buffer
	do := func(stdin string, args ...string) int {
		var out bytes.Buffer
		code := Run(append([]string{args[0], "--cwd", root}, args[1:]...), strings.NewReader(stdin), &out, &errb, "t")
		captured.Write(out.Bytes())
		return code
	}
	if do("", "init", "--consent") != 0 {
		t.Fatal("init")
	}
	events := []map[string]any{
		{"id": "e1", "session_id": "s", "category": "user_message", "content": "deploy with " + awsKey + " please"},
		{"id": "e2", "session_id": "s", "category": "tool_result", "paths": []string{"cfg/.env"}, "content": "DB_PASSWORD=" + envValue},
		{"id": "e3", "session_id": "s", "category": "tool_result", "content": "-----BEGIN PRIVATE KEY-----\n" + pemBody + "\n-----END PRIVATE KEY-----"},
		{"id": "e4", "session_id": "s", "category": "assistant_message", "content": "done fixing lexer.go"},
		{"id": "e5", "session_id": "s", "category": "private_reasoning", "content": "secret chain of thought " + envValue},
		{"id": "e6", "session_id": "s", "category": "task_boundary"},
	}
	body, _ := json.Marshal(map[string]any{"events": events})
	if code := do(string(body), "ingest"); code != 1 { // e5 rejected
		t.Fatalf("ingest exit %d", code)
	}
	if do("", "maintain", "--flush") != 0 {
		t.Fatal("maintain")
	}
	var entry struct{ Memory struct{ ID string } }
	var listed bytes.Buffer
	Run([]string{"list", "--cwd", root}, strings.NewReader(""), &listed, &errb, "t")
	captured.Write(listed.Bytes())
	if err := json.Unmarshal(listed.Bytes(), &entry); err != nil {
		t.Fatalf("list: %v %q", err, listed.String())
	}
	id := entry.Memory.ID
	for _, c := range [][]string{{"pin", id}, {"archive", id}, {"restore", id}, {"show", id}, {"gaps"}} {
		if do("", c...) != 0 {
			t.Fatalf("%v failed: %s", c, errb.String())
		}
	}
	for _, c := range [][]string{{"index"}, {"memory-search", "--query", "deploy lexer", "--engine", "fts"}, {"read", "--indexed", "e1"}, {"read", "--indexed", "e2"}} {
		if do("", c...) != 0 {
			t.Fatalf("%v failed: %s", c, errb.String())
		}
	}

	leaks := []string{awsKey, envValue, pemBody, "chain of thought"}
	check := func(where string, data []byte) {
		for _, l := range leaks {
			if bytes.Contains(data, []byte(l)) {
				t.Errorf("%q leaked into %s", l, where)
			}
		}
	}
	check("command output", captured.Bytes())
	check("stderr", errb.Bytes())
	store := filepath.Join(root, ".curator")
	files := 0
	filepath.Walk(store, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		files++
		data, _ := os.ReadFile(p)
		if strings.HasSuffix(p, ".gz") {
			data = gunzip(t, data)
		}
		check(p, data)
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			t.Errorf("%s has group/other access: %v", p, mode)
		}
		return nil
	})
	if files < 4 {
		t.Fatalf("scanned only %d files; the store surfaces were not produced", files)
	}
	// Non-secret evidence is still captured, and the store is excluded from Git.
	if j, _ := os.ReadFile(filepath.Join(store, "journal", "events.jsonl")); !strings.Contains(string(j), "done fixing lexer.go") {
		t.Fatal("ordinary content was lost")
	}
	if out, err := exec.Command("git", "-C", root, "status", "--porcelain", "--untracked-files=all").CombinedOutput(); err != nil || strings.Contains(string(out), ".curator/") {
		t.Fatalf("store visible to git: %v %s", err, out)
	}
	if info, err := os.Stat(store); err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("store dir permissions: %v %v", info, err)
	}
}

func gunzip(t *testing.T, data []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
