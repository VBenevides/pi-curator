package cli

import (
	"errors"
	"fmt"
	"io"

	"pi-curator/curator/internal/ingest"
)

// ingestCmd reads one JSON request from stdin and prints one JSON response.
// The response is printed whenever a request was understood, so a caller can
// tell per event what is durable; the exit status is 1 unless every event is
// durable or an already-durable duplicate.
func (e env) ingestCmd(args []string) int {
	fs := e.flags("ingest")
	cwd := cwdFlag(fs)
	if !e.parse(fs, args) {
		return exitUsage
	}
	dir, err := resolveCwd(*cwd)
	if err != nil {
		return e.failure("resolve directory", err)
	}
	data, err := io.ReadAll(io.LimitReader(e.stdin, ingest.MaxRequestBytes+1))
	if err != nil {
		return e.failure("read request", err)
	}
	if len(data) > ingest.MaxRequestBytes {
		return e.usageError(fmt.Sprintf("request exceeds %d bytes", ingest.MaxRequestBytes))
	}
	req, err := ingest.ParseRequest(data)
	if err != nil {
		return e.usageError(err.Error())
	}
	resp := ingest.Run(dir, req, ingest.Options{})
	if code := e.json(resp); code != exitOK {
		return code
	}
	for _, r := range append(resp.Results, resp.Gaps...) {
		if r.Outcome != ingest.OutcomeDurable && r.Outcome != ingest.OutcomeDuplicate {
			return e.failure("ingest", errors.New("not every event or gap is durable; see per-item results"))
		}
	}
	return exitOK
}
