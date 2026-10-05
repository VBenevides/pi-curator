package curator

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestVersionComesFromVersionFile(t *testing.T) {
	raw, err := os.ReadFile("VERSION")
	if err != nil {
		t.Fatal(err)
	}
	if Version != strings.TrimSpace(string(raw)) {
		t.Fatalf("Version = %q, VERSION file = %q", Version, raw)
	}
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(Version) {
		t.Fatalf("Version %q is not MAJOR.MINOR.PATCH", Version)
	}
}
