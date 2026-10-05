// Package onboard implements consent-gated repository memory initialization.
// Inspect is read-only; Initialize runs only after the caller obtained
// explicit user consent and never holds the lock while waiting for input.
package onboard

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"pi-curator/curator/internal/lock"
	"pi-curator/curator/internal/repo"
)

// State is the onboarding state reported to the plugin.
type State string

const (
	// StatePendingConsent: no .curator yet; ask before creating it.
	StatePendingConsent State = "pending_consent"
	// StateNeedsIgnore: .curator exists but Git would not ignore it; ask
	// before adding the rule to .git/info/exclude.
	StateNeedsIgnore State = "needs_ignore"
	// StateTracked: Git tracks files under .curator; recording stays disabled
	// until the user untracks them.
	StateTracked State = "tracked"
	// StateReady: .curator exists, is ignored, and has no tracked files.
	StateReady State = "ready"
	// StateNotGit: outside a Git repository; no onboarding is offered.
	StateNotGit State = "not_git"
)

// Rule is the root-scoped ignore line curator adds.
const Rule = "/.curator/"

// Status is a snapshot of the onboarding state.
type Status struct {
	State   State  `json:"state"`
	Root    string `json:"root,omitempty"`
	Store   string `json:"store,omitempty"`
	RepoID  string `json:"repo_id,omitempty"`
	Message string `json:"message,omitempty"`
}

// probePath is a representative path inside the store used for ignore checks.
const probePath = ".curator/memory.jsonl.lock"

// ErrTracked is returned when initialization is blocked by tracked memory.
var ErrTracked = errors.New("files under .curator are tracked by Git")

func git(root string, args ...string) (int, string, error) {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), out.String() + errb.String(), nil
	}
	if err != nil {
		return -1, "", fmt.Errorf("run git: %w", err)
	}
	return 0, out.String(), nil
}

// ignored asks Git whether the store is effectively excluded, honoring every
// ignore source. --no-index makes already-tracked paths report the rule.
func ignored(root string) (bool, error) {
	code, out, err := git(root, "check-ignore", "-q", "--no-index", "--", probePath)
	if err != nil {
		return false, err
	}
	switch code {
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	return false, fmt.Errorf("git check-ignore failed (%d): %s", code, strings.TrimSpace(out))
}

func tracked(root string) (bool, error) {
	code, out, err := git(root, "ls-files", "--", ".curator")
	if err != nil {
		return false, err
	}
	if code != 0 {
		return false, fmt.Errorf("git ls-files failed (%d): %s", code, strings.TrimSpace(out))
	}
	return strings.TrimSpace(out) != "", nil
}

// Inspect reports the onboarding state of the repository containing dir
// without changing any file.
func Inspect(dir string) (Status, error) {
	info, err := repo.Resolve(dir)
	if errors.Is(err, repo.ErrNotGit) {
		return Status{State: StateNotGit, Message: "not inside a Git repository; memory is not offered"}, nil
	}
	if err != nil {
		return Status{}, err
	}
	return inspectInfo(info)
}

func inspectInfo(info repo.Info) (Status, error) {
	st := Status{Root: info.Root, Store: info.StoreDir()}
	id, err := info.ID()
	if err != nil {
		return Status{}, err
	}
	st.RepoID = id
	fi, err := os.Lstat(info.StoreDir())
	switch {
	case errors.Is(err, os.ErrNotExist):
		st.State = StatePendingConsent
		return st, nil
	case err != nil:
		return Status{}, fmt.Errorf("stat store: %w", err)
	case !fi.IsDir():
		return Status{}, fmt.Errorf("%s exists and is not a directory", info.StoreDir())
	}
	isTracked, err := tracked(info.Root)
	if err != nil {
		return Status{}, err
	}
	if isTracked {
		st.State = StateTracked
		st.Message = ".gitignore does not untrack files; run `git rm -r --cached .curator` yourself, then restart"
		return st, nil
	}
	isIgnored, err := ignored(info.Root)
	if err != nil {
		return Status{}, err
	}
	if !isIgnored {
		st.State = StateNeedsIgnore
		return st, nil
	}
	st.State = StateReady
	return st, nil
}

// Initialize creates or repairs the store after consent. It returns the
// final status; any state other than StateReady comes with an error, so a
// partial initialization is never reported as ready. Safe to retry.
func Initialize(dir string) (Status, error) {
	info, err := repo.Resolve(dir)
	if err != nil {
		return Status{}, err
	}
	before, err := inspectInfo(info)
	if err != nil {
		return Status{}, err
	}
	if before.State == StateTracked {
		return before, ErrTracked
	}
	if err := os.Mkdir(info.StoreDir(), 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return Status{}, fmt.Errorf("create %s: %w", info.StoreDir(), err)
	}
	// The directory now serializes concurrent initializers through its lock.
	err = lock.With(filepath.Join(info.StoreDir(), lock.FileName), lock.DefaultTimeout, func(*lock.Held) error {
		return apply(info)
	})
	if err != nil {
		return Status{}, fmt.Errorf("initialize memory in %s: %w", info.Root, err)
	}
	after, err := inspectInfo(info)
	if err != nil {
		return Status{}, err
	}
	if after.State != StateReady {
		return after, fmt.Errorf("initialization incomplete: state %s", after.State)
	}
	return after, nil
}

// apply runs under the lock; it re-checks state so retries and concurrent
// initializers never duplicate an effective ignore rule.
func apply(info repo.Info) error {
	st, err := inspectInfo(info)
	if err != nil {
		return err
	}
	if st.State == StateTracked {
		return ErrTracked
	}
	if st.State == StateNeedsIgnore {
		path, err := excludePath(info.Root)
		if err != nil {
			return err
		}
		if err := appendRule(path); err != nil {
			return err
		}
	}
	idPath := filepath.Join(info.StoreDir(), "identity")
	if _, err := os.Stat(idPath); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(idPath, []byte(info.DerivedID+"\n"), 0o600); err != nil {
			return fmt.Errorf("write repository identity: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("stat repository identity: %w", err)
	}
	return nil
}

// excludePath is the repository-local ignore file (.git/info/exclude, resolved
// by Git so linked worktrees work). Writing there leaves tracked files alone.
func excludePath(root string) (string, error) {
	code, out, err := git(root, "rev-parse", "--path-format=absolute", "--git-path", "info/exclude")
	if err != nil {
		return "", fmt.Errorf("locate .git/info/exclude: %w", err)
	}
	if code != 0 || strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("locate .git/info/exclude: git exited %d", code)
	}
	return strings.TrimSpace(out), nil
}

// appendRule adds Rule to the exclude file, creating it when absent and
// keeping all existing content.
func appendRule(path string) error {
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	add := Rule + "\n"
	if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		add = "\n" + add
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := f.WriteString(add); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}
