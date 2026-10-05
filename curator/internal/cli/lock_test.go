package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pi-curator/curator/internal/lock"
)

func snapshotStore(t *testing.T, store string) string {
	t.Helper()
	var sb strings.Builder
	filepath.Walk(store, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && filepath.Base(p) != lock.FileName {
			b, _ := os.ReadFile(p)
			sb.WriteString(p + "=" + string(b) + "\n")
		}
		return nil
	})
	return sb.String()
}

func TestEveryMutatingCommandTimesOutWithoutChangingState(t *testing.T) {
	old := lockTimeout
	lockTimeout = 100 * time.Millisecond
	t.Cleanup(func() { lockTimeout = old })

	root := gitInit(t)
	var errb bytes.Buffer
	run([]string{"init", "--consent", "--cwd", root}, io.Discard, &errb)
	body := `{"events":[{"id":"e1","session_id":"s","category":"user_message","content":"x"},{"id":"e2","session_id":"s","category":"task_boundary"}]}`
	Run([]string{"ingest", "--cwd", root}, strings.NewReader(body), io.Discard, &errb, "t")
	run([]string{"maintain", "--cwd", root}, io.Discard, &errb)
	store := filepath.Join(root, ".curator")

	h, err := lock.Acquire(filepath.Join(store, lock.FileName), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	before := snapshotStore(t, store)

	cmds := [][]string{
		{"maintain", "--flush", "--cwd", root},
		{"recover", "--cwd", root},
		{"pin", "--cwd", root, "mem_x"},
	}
	for _, c := range cmds {
		var out, e bytes.Buffer
		if code := run(c, &out, &e); code != 1 {
			t.Errorf("%v: exit %d (stderr %q)", c, code, e.String())
		}
	}
	var out bytes.Buffer
	if code := Run([]string{"ingest", "--cwd", root}, strings.NewReader(strings.Replace(body, "e1", "e3", 1)), &out, &errb, "t"); code != 1 || !strings.Contains(out.String(), "lock_timeout") {
		t.Errorf("ingest under lock: %d %s", code, out.String())
	}
	if after := snapshotStore(t, store); after != before {
		t.Fatalf("state changed while the lock was held elsewhere")
	}
	// Read-only commands do not need the lock.
	for _, c := range [][]string{{"list", "--cwd", root}, {"gaps", "--cwd", root}, {"maintain", "--dry-run", "--cwd", root}} {
		if code := run(c, io.Discard, &errb); code != 0 {
			t.Errorf("%v under lock: %d %s", c, code, errb.String())
		}
	}
}
