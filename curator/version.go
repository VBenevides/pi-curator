// Package curator exposes module-level metadata.
package curator

import (
	_ "embed"
	"strings"
)

//go:embed VERSION
var rawVersion string

// Version is the contents of curator/VERSION, the single source of truth.
var Version = strings.TrimSpace(rawVersion)
