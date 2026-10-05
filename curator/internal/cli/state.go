package cli

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"pi-curator/curator/internal/journal"
	"pi-curator/curator/internal/lock"
	"pi-curator/curator/internal/memory"
	"pi-curator/curator/internal/record"
	"pi-curator/curator/internal/redact"
)

func currentBranch(root string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "git", "-C", root, "symbolic-ref", "--quiet", "--short", "HEAD").Output()
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok || exit.ExitCode() != 1 {
			return "", fmt.Errorf("resolve branch: %w", err)
		}
		output, err = exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--verify", "HEAD").Output()
		if err != nil {
			return "", fmt.Errorf("resolve detached HEAD: %w", err)
		}
		output = append([]byte("detached:"), output...)
	}
	branch := strings.TrimSpace(string(output))
	if branch == "" || len(branch) > 256 {
		return "", fmt.Errorf("invalid branch namespace")
	}
	return branch, nil
}

func (e env) stateCmd(args []string) int {
	fs := e.flags("state")
	cwd := cwdFlag(fs)
	key := fs.String("key", "", "state key (identifier)")
	source := fs.String("source", "", "stored event ID")
	branch := fs.String("branch", "", "declared branch, default current HEAD")
	global := fs.Bool("repository-wide", false, "declare this evidence applicable across branches")
	if !e.parse(fs, args) {
		return exitUsage
	}
	if *key == "" || *source == "" || (*global && *branch != "") {
		return e.usageError("state needs --key and --source; branch and repository-wide are exclusive")
	}
	st, code := e.readyStore(*cwd, "state")
	if code != exitOK {
		return code
	}
	scope := record.Scope{Repository: st.RepoID, Branch: *branch}
	var err error
	if !*global && scope.Branch == "" {
		scope.Branch, err = currentBranch(st.Root)
		if err != nil {
			return e.failure("state", err)
		}
	}
	policy, err := redact.Load(st.Store)
	if err != nil {
		return e.failure("state", err)
	}
	if policy.Apply(*key, nil, false).Redactions > 0 || policy.Apply(scope.Branch, nil, false).Redactions > 0 {
		return e.usageError("state namespace contains sensitive material")
	}
	var fact *record.Memory
	err = lock.With(filepath.Join(st.Store, lock.FileName), lockTimeout, func(h *lock.Held) error {
		fact, err = memory.RegisterState(h, journal.New(st.Store), st.Store, *key, scope, *source, time.Now())
		return err
	})
	if err != nil {
		return e.failure("state", err)
	}
	return e.json(fact)
}
