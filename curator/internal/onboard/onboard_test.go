package onboard

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func newRepo(t *testing.T) string {
	t.Helper()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	run(t, root, "init", "-q", "-b", "main")
	return root
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustInit(t *testing.T, dir string) Status {
	t.Helper()
	st, err := Initialize(dir)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return st
}

func TestInspectChangesNothingAndAsksFirst(t *testing.T) {
	root := newRepo(t)
	sub := filepath.Join(root, "a", "b")
	os.MkdirAll(sub, 0o755)
	st, err := Inspect(sub)
	if err != nil || st.State != StatePendingConsent || st.Root != root {
		t.Fatalf("status %+v, %v", st, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".curator")); !os.IsNotExist(err) {
		t.Fatal("Inspect created .curator")
	}
	if _, err := os.Stat(filepath.Join(root, ".gitignore")); !os.IsNotExist(err) {
		t.Fatal("Inspect created .gitignore")
	}
}

func TestInitializeFromSubdirectoryCreatesRootStoreAndIgnore(t *testing.T) {
	root := newRepo(t)
	sub := filepath.Join(root, "pkg")
	os.MkdirAll(sub, 0o755)
	st := mustInit(t, sub)
	if st.State != StateReady || st.Store != filepath.Join(root, ".curator") {
		t.Fatalf("status %+v", st)
	}
	if got := read(t, filepath.Join(root, ".git", "info", "exclude")); !strings.HasSuffix(got, "/.curator/\n") {
		t.Fatalf("exclude = %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, ".gitignore")); !os.IsNotExist(err) {
		t.Fatal("tracked .gitignore was created")
	}
	if _, err := os.Stat(filepath.Join(sub, ".curator")); !os.IsNotExist(err) {
		t.Fatal("store created outside the root")
	}
	if fi, _ := os.Stat(st.Store); fi.Mode().Perm() != 0o700 {
		t.Fatalf("store mode %v", fi.Mode().Perm())
	}
	if out := run(t, root, "status", "--porcelain"); strings.Contains(out, ".curator") {
		t.Fatalf("store visible to git: %s", out)
	}
}

func TestExistingIgnoreFileIsPreservedAndRuleNotDuplicated(t *testing.T) {
	root := newRepo(t)
	ex := filepath.Join(root, ".git", "info", "exclude")
	os.WriteFile(ex, []byte("node_modules/\ndist"), 0o644) // no trailing newline
	mustInit(t, root)
	want := "node_modules/\ndist\n/.curator/\n"
	if got := read(t, ex); got != want {
		t.Fatalf("exclude = %q, want %q", got, want)
	}
	mustInit(t, root) // retry is idempotent
	if got := read(t, ex); got != want {
		t.Fatalf("retry changed exclude: %q", got)
	}
}

func TestExistingEffectiveRuleIsReusedWithoutEditingIgnore(t *testing.T) {
	root := newRepo(t)
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".curator\n"), 0o644)
	os.Mkdir(filepath.Join(root, ".curator"), 0o700)
	os.WriteFile(filepath.Join(root, ".curator", "keep.txt"), []byte("data"), 0o600)
	st, _ := Inspect(root)
	if st.State != StateReady {
		t.Fatalf("existing ignored store: %+v", st)
	}
	mustInit(t, root)
	if got := read(t, filepath.Join(root, ".gitignore")); got != ".curator\n" {
		t.Fatalf("duplicate rule added: %q", got)
	}
	if read(t, filepath.Join(root, ".curator", "keep.txt")) != "data" {
		t.Fatal("existing memory content changed")
	}
}

func TestUnignoredExistingStoreNeedsConsentAndKeepsData(t *testing.T) {
	root := newRepo(t)
	os.Mkdir(filepath.Join(root, ".curator"), 0o700)
	os.WriteFile(filepath.Join(root, ".curator", "keep.txt"), []byte("data"), 0o600)
	if st, _ := Inspect(root); st.State != StateNeedsIgnore {
		t.Fatalf("state %s", st.State)
	}
	if _, err := os.Stat(filepath.Join(root, ".gitignore")); !os.IsNotExist(err) {
		t.Fatal("Inspect edited .gitignore")
	}
	mustInit(t, root)
	if read(t, filepath.Join(root, ".curator", "keep.txt")) != "data" {
		t.Fatal("stored memory replaced")
	}
}

func TestIneffectiveRuleIsNotTrusted(t *testing.T) {
	root := newRepo(t)
	// A rule scoped to a different directory does not exclude the root store.
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte("sub/.curator/\n"), 0o644)
	os.Mkdir(filepath.Join(root, ".curator"), 0o700)
	if st, _ := Inspect(root); st.State != StateNeedsIgnore {
		t.Fatalf("state %s", st.State)
	}
	mustInit(t, root)
}

func TestTrackedMemoryBlocksInitializationAndIsNeverUntracked(t *testing.T) {
	root := newRepo(t)
	os.Mkdir(filepath.Join(root, ".curator"), 0o700)
	os.WriteFile(filepath.Join(root, ".curator", "secret.jsonl"), []byte("x"), 0o600)
	run(t, root, "add", "-f", ".curator/secret.jsonl")
	st, err := Initialize(root)
	if !errors.Is(err, ErrTracked) || st.State != StateTracked {
		t.Fatalf("status %+v, err %v", st, err)
	}
	if !strings.Contains(run(t, root, "ls-files", ".curator"), "secret.jsonl") {
		t.Fatal("curator untracked the file")
	}
	if strings.Contains(read(t, filepath.Join(root, ".git", "info", "exclude")), ".curator") {
		t.Fatal("exclude file edited while tracked")
	}
}

func TestWriteFailureIsReportedNotReadyAndRetryable(t *testing.T) {
	root := newRepo(t)
	gi := filepath.Join(root, ".git", "info", "exclude")
	os.Remove(gi)
	os.MkdirAll(gi, 0o755) // the exclude file cannot be opened for append
	if _, err := Initialize(root); err == nil {
		t.Fatal("write failure reported as success")
	}
	if st, _ := Inspect(root); st.State == StateReady {
		t.Fatal("partial initialization reported ready")
	}
	os.Remove(gi)
	if st := mustInit(t, root); st.State != StateReady {
		t.Fatalf("retry state %s", st.State)
	}
}

func TestWorktreeGetsItsOwnRootStore(t *testing.T) {
	root := newRepo(t)
	run(t, root, "commit", "-q", "--allow-empty", "-m", "init")
	wt := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-wt")
	run(t, root, "worktree", "add", "-q", wt)
	st := mustInit(t, filepath.Join(wt))
	if st.Root != wt || st.State != StateReady {
		t.Fatalf("status %+v", st)
	}
	if _, err := os.Stat(filepath.Join(root, ".curator")); !os.IsNotExist(err) {
		t.Fatal("worktree init touched the main checkout")
	}
	if !strings.Contains(read(t, filepath.Join(root, ".git", "info", "exclude")), "/.curator/\n") {
		t.Fatal("shared exclude file not written")
	}
	if out := run(t, wt, "status", "--porcelain"); strings.Contains(out, ".curator") {
		t.Fatalf("worktree store visible to git: %s", out)
	}
}

func TestNonGitDirectoryIsNeverInitialized(t *testing.T) {
	dir := t.TempDir()
	st, err := Inspect(dir)
	if err != nil || st.State != StateNotGit {
		t.Fatalf("status %+v, %v", st, err)
	}
	if _, err := Initialize(dir); err == nil {
		t.Fatal("initialized outside Git")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("files created outside Git: %v", entries)
	}
}

func TestConcurrentInitializersAddOneRule(t *testing.T) {
	root := newRepo(t)
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := Initialize(root); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := strings.Count(read(t, filepath.Join(root, ".git", "info", "exclude")), "/.curator/"); got != 1 {
		t.Fatalf("ignore rule written %d times", got)
	}
}
