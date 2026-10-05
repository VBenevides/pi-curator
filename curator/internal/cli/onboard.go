package cli

import (
	"errors"
	"flag"
	"os"

	"pi-curator/curator/internal/onboard"
)

func cwdFlag(fs *flag.FlagSet) *string {
	return fs.String("cwd", "", "directory to resolve the repository from (default: current directory)")
}

func resolveCwd(cwd string) (string, error) {
	if cwd != "" {
		return cwd, nil
	}
	return os.Getwd()
}

func (e env) status(args []string) int {
	fs := e.flags("status")
	cwd := cwdFlag(fs)
	if !e.parse(fs, args) {
		return exitUsage
	}
	dir, err := resolveCwd(*cwd)
	if err != nil {
		return e.failure("resolve directory", err)
	}
	st, err := onboard.Inspect(dir)
	if err != nil {
		return e.failure("status", err)
	}
	return e.json(st)
}

func (e env) initRepo(args []string) int {
	fs := e.flags("init")
	cwd := cwdFlag(fs)
	consent := fs.Bool("consent", false, "the user explicitly agreed to create .curator and ignore it")
	if !e.parse(fs, args) {
		return exitUsage
	}
	if !*consent {
		return e.usageError("init requires --consent: ask the user before creating .curator")
	}
	dir, err := resolveCwd(*cwd)
	if err != nil {
		return e.failure("resolve directory", err)
	}
	st, err := onboard.Initialize(dir)
	if errors.Is(err, onboard.ErrTracked) {
		e.json(st)
		return e.failure("init", err)
	}
	if err != nil {
		return e.failure("init", err)
	}
	return e.json(st)
}
