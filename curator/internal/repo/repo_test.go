package repo

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func mkdir(t *testing.T, parts ...string) string {
	t.Helper()
	p := filepath.Join(parts...)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixture builds a main checkout with a linked worktree, as git lays them out.
func fixture(t *testing.T) (main, worktree string) {
	t.Helper()
	base, _ := filepath.EvalSymlinks(t.TempDir())
	main = mkdir(t, base, "main")
	mkdir(t, main, ".git", "worktrees", "wt")
	worktree = mkdir(t, base, "wt")
	gitdir := filepath.Join(main, ".git", "worktrees", "wt")
	write(t, filepath.Join(worktree, ".git"), "gitdir: "+gitdir+"\n")
	write(t, filepath.Join(gitdir, "commondir"), "../..\n")
	return main, worktree
}

func TestSubdirectoriesResolveToRootWithSameIdentity(t *testing.T) {
	main, _ := fixture(t)
	sub := mkdir(t, main, "a", "b")
	root, err := Resolve(main)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Resolve(sub)
	if err != nil {
		t.Fatal(err)
	}
	if got.Root != main || got.DerivedID != root.DerivedID || got.Worktree {
		t.Fatalf("subdir resolved to %+v, want root %s id %s", got, main, root.DerivedID)
	}
}

func TestWorktreeHasOwnRootAndSharesRepositoryIdentity(t *testing.T) {
	main, wt := fixture(t)
	m, _ := Resolve(main)
	w, err := Resolve(mkdir(t, wt, "pkg"))
	if err != nil {
		t.Fatal(err)
	}
	if w.Root != wt || !w.Worktree {
		t.Fatalf("worktree resolved to %+v", w)
	}
	if w.CommonDir != m.CommonDir || w.DerivedID != m.DerivedID {
		t.Fatalf("worktree identity %+v differs from main %+v", w, m)
	}
	if w.StoreDir() != filepath.Join(wt, ".curator") {
		t.Fatalf("store dir %s", w.StoreDir())
	}
}

func TestDistinctRepositoriesGetDistinctIdentities(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	a := mkdir(t, base, "a", ".git")
	b := mkdir(t, base, "b", ".git")
	ra, _ := Resolve(filepath.Dir(a))
	rb, _ := Resolve(filepath.Dir(b))
	if ra.DerivedID == rb.DerivedID {
		t.Fatal("different repositories share an identity")
	}
}

func TestNonGitDirectoryIsRejected(t *testing.T) {
	if _, err := Resolve(t.TempDir()); !errors.Is(err, ErrNotGit) {
		t.Fatalf("err = %v, want ErrNotGit", err)
	}
}

func TestMovedStoreKeepsPersistedIdentity(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	old := mkdir(t, base, "old", ".git")
	oldRoot := filepath.Dir(old)
	r, _ := Resolve(oldRoot)
	mkdir(t, r.StoreDir())
	write(t, filepath.Join(r.StoreDir(), identityFile), r.DerivedID+"\n")

	moved := filepath.Join(base, "moved")
	if err := os.Rename(oldRoot, moved); err != nil {
		t.Fatal(err)
	}
	m, err := Resolve(moved)
	if err != nil {
		t.Fatal(err)
	}
	if m.DerivedID == r.DerivedID {
		t.Fatal("test setup: derived identity should change on move")
	}
	id, err := m.ID()
	if err != nil || id != r.DerivedID {
		t.Fatalf("ID() = %q, %v; want persisted %q", id, err, r.DerivedID)
	}
}

func TestEmptyIdentityFileIsAnError(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	root := filepath.Dir(mkdir(t, base, "r", ".git"))
	r, _ := Resolve(root)
	mkdir(t, r.StoreDir())
	write(t, filepath.Join(r.StoreDir(), identityFile), "\n")
	if _, err := r.ID(); err == nil {
		t.Fatal("empty identity accepted")
	}
}
