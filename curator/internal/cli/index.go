package cli

import (
	"path/filepath"
	"pi-curator/curator/internal/index"
	"pi-curator/curator/internal/lock"
)

func (e env) indexCmd(args []string) int {
	fs := e.flags("index")
	cwd := cwdFlag(fs)
	rebuild := fs.Bool("rebuild", false, "rebuild the disposable sidecar")
	if !e.parse(fs, args) {
		return exitUsage
	}
	st, code := e.readyStore(*cwd, "index")
	if code != exitOK {
		return code
	}
	var result index.Progress
	err := lock.With(filepath.Join(st.Store, lock.FileName), lockTimeout, func(_ *lock.Held) error {
		idx, err := index.Open(st.Store)
		if err != nil {
			return err
		}
		defer idx.Close()
		result, err = idx.Update(*rebuild)
		return err
	})
	if err != nil {
		return e.failure("index", err)
	}
	return e.json(result)
}
