package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(args []string, stdout, stderr io.Writer) int {
	return Run(args, strings.NewReader(""), stdout, stderr, "test")
}

func TestInvalidUsageDoesNotRunCommand(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "unsupported command", args: []string{"frobnicate"}},
		{name: "unknown flag", args: []string{"--unknown"}},
		{name: "extra help argument", args: []string{"--help", "extra"}},
		{name: "extra version argument", args: []string{"--version", "extra"}},
		{name: "conflicting flags", args: []string{"--help", "--version"}},
		{name: "status positional", args: []string{"status", "extra"}},
		{name: "status unknown flag", args: []string{"status", "--nope"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout bytes.Buffer
			if code := run(tt.args, &stdout, io.Discard); code != 2 {
				t.Fatalf("exit status = %d, want usage failure (2)", code)
			}
			if stdout.Len() != 0 {
				t.Fatalf("invalid invocation produced command output: %q", stdout.String())
			}
		})
	}
}

func TestOutputFailureReturnsFailure(t *testing.T) {
	for _, command := range []string{"--help", "--version"} {
		t.Run(command, func(t *testing.T) {
			if code := run([]string{command}, failingWriter{}, io.Discard); code != 1 {
				t.Fatalf("exit status = %d, want output failure (1)", code)
			}
		})
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("output unavailable")
}

func gitInit(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return root
}

func TestInitRequiresConsentAndStatusIsReadOnly(t *testing.T) {
	root := gitInit(t)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"init", "--cwd", root}, &stdout, &stderr); code != 2 {
		t.Fatalf("init without consent exit = %d, stderr %q", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(root, ".curator")); !os.IsNotExist(err) {
		t.Fatal("init without consent created .curator")
	}

	var st struct{ State string }
	if code := run([]string{"status", "--cwd", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("status exit = %d", code)
	}
	if err := json.Unmarshal(stdout.Bytes(), &st); err != nil || st.State != "pending_consent" {
		t.Fatalf("status output %q: %v", stdout.String(), err)
	}
	if _, err := os.Stat(filepath.Join(root, ".curator")); !os.IsNotExist(err) {
		t.Fatal("status created .curator")
	}

	stdout.Reset()
	if code := run([]string{"init", "--consent", "--cwd", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("init exit = %d, stderr %q", code, stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &st); err != nil || st.State != "ready" {
		t.Fatalf("init output %q: %v", stdout.String(), err)
	}
}

func TestInitOutsideGitFailsWithoutFiles(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"init", "--consent", "--cwd", dir}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit = %d", code)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("created %v outside Git", entries)
	}
}

func TestIngestExitStatusReflectsDurability(t *testing.T) {
	root := gitInit(t)
	var out, errb bytes.Buffer
	if code := run([]string{"init", "--consent", "--cwd", root}, &out, &errb); code != 0 {
		t.Fatalf("init: %d %s", code, errb.String())
	}
	body := `{"request_id":"r1","events":[{"id":"e1","session_id":"s","category":"user_message","content":"hi"}]}`
	out.Reset()
	if code := Run([]string{"ingest", "--cwd", root}, strings.NewReader(body), &out, &errb, "t"); code != 0 {
		t.Fatalf("ingest: %d %s %s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), `"outcome":"durable"`) {
		t.Fatalf("stdout: %s", out.String())
	}
	bad := `{"events":[{"id":"bad id","session_id":"s","category":"user_message"}]}`
	out.Reset()
	if code := Run([]string{"ingest", "--cwd", root}, strings.NewReader(bad), &out, &errb, "t"); code != 1 || !strings.Contains(out.String(), `"rejected"`) {
		t.Fatalf("rejected event: %d %s", code, out.String())
	}
	if code := Run([]string{"ingest", "--cwd", root}, strings.NewReader("{"), &out, &errb, "t"); code != 2 {
		t.Fatalf("malformed request: %d", code)
	}
}

func TestMaintainPublishesGrepReadableViewFromIngestedEvents(t *testing.T) {
	root := gitInit(t)
	var out, errb bytes.Buffer
	if code := run([]string{"init", "--consent", "--cwd", root}, &out, &errb); code != 0 {
		t.Fatalf("init: %d %s", code, errb.String())
	}
	body := `{"events":[{"id":"e1","session_id":"s","category":"user_message","content":"fix lexer.go"},{"id":"e2","session_id":"s","category":"assistant_message","content":"done"}]}`
	if code := Run([]string{"ingest", "--cwd", root}, strings.NewReader(body), &out, &errb, "t"); code != 0 {
		t.Fatalf("ingest: %d", code)
	}
	out.Reset()
	if code := run([]string{"maintain", "--flush", "--cwd", root}, &out, &errb); code != 0 || !strings.Contains(out.String(), `"active":1`) {
		t.Fatalf("maintain: %d %s %s", code, out.String(), errb.String())
	}
	data, err := os.ReadFile(filepath.Join(root, ".curator", "memory", "active.jsonl"))
	if err != nil || !strings.Contains(string(data), "fix lexer.go") {
		t.Fatalf("active view: %v %q", err, data)
	}
	if code := run([]string{"maintain", "--cwd", t.TempDir()}, io.Discard, io.Discard); code != 1 {
		t.Fatalf("maintain outside a ready repo: %d", code)
	}
}
