package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFeedbackIsExplicitIdempotentAndEpisodeScoped(t *testing.T) {
	root := gitInit(t)
	setup(t, root, "s", "fix lexer")
	var out bytes.Buffer
	run([]string{"list", "--cwd", root}, &out, io.Discard)
	mem := between(out.String(), `"id":"`, `"`)
	fb := func(args ...string) (int, string) {
		var o, e bytes.Buffer
		code := run(append([]string{"feedback", "--cwd", root}, args...), &o, &e)
		return code, o.String() + e.String()
	}
	if code, o := fb("--label", "misleading", "--source", "user_correction", "--note", "key AKIAIOSFODNN7EXAMPLE", mem); code != 0 || !strings.Contains(o, `"changed":true`) {
		t.Fatalf("add: %d %s", code, o)
	}
	if code, o := fb("--label", "misleading", "--source", "user_correction", "--note", "key AKIAIOSFODNN7EXAMPLE", mem); code != 0 || !strings.Contains(o, `"changed":false`) {
		t.Fatalf("repeat should be a no-op: %d %s", code, o)
	}
	for name, args := range map[string][]string{
		"unknown episode": {"--label", "useful", "--source", "benchmark", "mem_nope"},
		"unknown label":   {"--label", "great", "--source", "benchmark", mem},
		"inferred source": {"--label", "useful", "--source", "read_count", mem},
		"missing label":   {"--source", "benchmark", mem},
	} {
		if code, _ := fb(args...); code == 0 {
			t.Errorf("%s accepted", name)
		}
	}
	code, o := fb("--list", mem)
	if code != 0 || strings.Count(o, "\n") != 1 || !strings.Contains(o, "misleading") {
		t.Fatalf("list: %d %s", code, o)
	}
	j, _ := os.ReadFile(filepath.Join(root, ".curator", "journal", "events.jsonl"))
	if strings.Contains(string(j), "AKIAIOSFODNN7EXAMPLE") {
		t.Fatal("secret persisted in feedback note")
	}
	// Feedback never changes memory state.
	if l := listOut(t, root); !strings.Contains(l, `"state":"active"`) {
		t.Fatalf("feedback altered lifecycle: %s", l)
	}
}
