// Package repo resolves the Git repository boundary and a stable identity for
// project memory, using only the filesystem (no git subprocess).
package repo

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrNotGit is returned when a directory is outside any Git repository.
var ErrNotGit = errors.New("not inside a Git repository")

// Info describes a resolved repository.
type Info struct {
	// Root is the top level of the working tree (a worktree's own root for
	// linked worktrees); memory storage lives at Root/.curator.
	Root string
	// CommonDir is the shared Git directory (the main repository's .git).
	CommonDir string
	// Worktree is true when Root is a linked worktree (.git is a file).
	Worktree bool
	// DerivedID is the identity derived from CommonDir: identical for the
	// main checkout and all of its worktrees, and for any subdirectory.
	DerivedID string
}

// Resolve finds the Git repository containing dir. It recognizes a .git
// directory and a linked-worktree .git file.
func Resolve(dir string) (Info, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Info{}, fmt.Errorf("resolve %q: %w", dir, err)
	}
	if abs, err = filepath.EvalSymlinks(abs); err != nil {
		return Info{}, fmt.Errorf("resolve %q: %w", dir, err)
	}
	for cur := abs; ; cur = filepath.Dir(cur) {
		gitPath := filepath.Join(cur, ".git")
		fi, statErr := os.Lstat(gitPath)
		switch {
		case statErr == nil && fi.IsDir():
			return build(cur, gitPath, false)
		case statErr == nil && fi.Mode().IsRegular():
			common, err := commonDirFromFile(cur, gitPath)
			if err != nil {
				return Info{}, err
			}
			return build(cur, common, true)
		case statErr != nil && !errors.Is(statErr, os.ErrNotExist):
			return Info{}, fmt.Errorf("stat %s: %w", gitPath, statErr)
		}
		if filepath.Dir(cur) == cur {
			return Info{}, ErrNotGit
		}
	}
}

func build(root, common string, worktree bool) (Info, error) {
	canon, err := filepath.EvalSymlinks(common)
	if err != nil {
		return Info{}, fmt.Errorf("resolve git dir %q: %w", common, err)
	}
	sum := sha256.Sum256([]byte(canon))
	return Info{Root: root, CommonDir: canon, Worktree: worktree, DerivedID: "repo_" + hex.EncodeToString(sum[:8])}, nil
}

// commonDirFromFile follows a worktree ".git" file ("gitdir: <path>") to the
// shared Git directory via the worktree's commondir file.
func commonDirFromFile(root, gitFile string) (string, error) {
	data, err := os.ReadFile(gitFile)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", gitFile, err)
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok {
		return "", fmt.Errorf("%s: missing gitdir line", gitFile)
	}
	gitdir := strings.TrimSpace(target)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(root, gitdir)
	}
	rel, err := os.ReadFile(filepath.Join(gitdir, "commondir"))
	if errors.Is(err, os.ErrNotExist) {
		return gitdir, nil // submodule-style gitdir without a shared common dir
	}
	if err != nil {
		return "", fmt.Errorf("read commondir in %s: %w", gitdir, err)
	}
	common := strings.TrimSpace(string(rel))
	if !filepath.IsAbs(common) {
		common = filepath.Join(gitdir, common)
	}
	return common, nil
}

// StoreDir is the memory storage directory for the repository.
func (i Info) StoreDir() string { return filepath.Join(i.Root, ".curator") }

const identityFile = "identity"

// ID returns the persisted repository identity when the store already holds
// one, so memory keeps its identity if the checkout is moved with its store.
// Otherwise it returns the derived identity.
func (i Info) ID() (string, error) {
	data, err := os.ReadFile(filepath.Join(i.StoreDir(), identityFile))
	if errors.Is(err, os.ErrNotExist) {
		return i.DerivedID, nil
	}
	if err != nil {
		return "", fmt.Errorf("read repository identity: %w", err)
	}
	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", fmt.Errorf("repository identity file %s is empty", identityFile)
	}
	return id, nil
}
