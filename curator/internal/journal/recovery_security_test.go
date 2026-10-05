package journal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRecoveryDoesNotOverwriteQuarantineSymlink(t *testing.T) {
	store := t.TempDir()
	j := New(store)
	if err := os.MkdirAll(filepath.Dir(j.path), 0700); err != nil {
		t.Fatal(err)
	}
	tail := []byte(`{"partial":`)
	if err := os.WriteFile(j.path, tail, 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, j.path+".tail-0"); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Recover(nil); err == nil {
		t.Fatal("quarantine symlink accepted")
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "preserve" {
		t.Fatalf("target changed: %q %v", got, err)
	}
	got, err = os.ReadFile(j.path)
	if err != nil || string(got) != string(tail) {
		t.Fatalf("source truncated: %q %v", got, err)
	}
}
