#!/usr/bin/env bash
# Local developer install: curator binary (go install) + capture plugin (omp plugin link).
# Re-run after changing the code or a VERSION file; it is idempotent.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

for tool in go omp; do
	command -v "$tool" >/dev/null || { echo "error: '$tool' not found on PATH" >&2; exit 1; }
done

# 1. curator binary -> $GOBIN or $(go env GOPATH)/bin
(cd "$root/curator" && go install ./cmd/curator)
bindir="$(go env GOBIN)"
bindir="${bindir:-$(go env GOPATH)/bin}"
echo "curator: $("$bindir/curator" --version) -> $bindir/curator"

case ":$PATH:" in
*":$bindir:"*) ;;
*)
	echo "warning: $bindir is not on PATH; OMP will not find curator." >&2
	echo "         add it to PATH, or export PI_CURATOR_BIN=$bindir/curator" >&2
	;;
esac

# 2. plugin -> linked into OMP (symlink, so edits apply without reinstalling)
omp plugin link --force "$root/plugin"
echo "plugin: pi-curator-plugin $(tr -d '[:space:]' <"$root/plugin/VERSION") linked from $root/plugin"

cat <<'EOF'

Done. Start OMP in a Git repository; on the first session it asks before creating .curator/.
Captured history: <repo>/.curator/journal/events.jsonl (raw, redacted) and, after
`curator maintain --flush`, <repo>/.curator/memory/active.jsonl (searchable episodes).
EOF
