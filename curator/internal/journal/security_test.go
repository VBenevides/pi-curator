package journal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSourceRefusesSymlinkDirectoriesAndFiles(t *testing.T) {
	for _, component := range []string{"store", "journal", "events.jsonl"} {
		t.Run(component, func(t *testing.T) {
			parent := t.TempDir()
			store := filepath.Join(parent, "store")
			path := filepath.Join(store, "journal", "events.jsonl")
			foreign := t.TempDir()
			if err := os.MkdirAll(filepath.Join(foreign, "journal"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(foreign, "journal", "events.jsonl"), []byte("sensitive"), 0600); err != nil {
				t.Fatal(err)
			}
			switch component {
			case "store":
				if err := os.Symlink(foreign, store); err != nil {
					t.Fatal(err)
				}
			case "journal":
				if err := os.Mkdir(store, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(foreign, "journal"), filepath.Join(store, "journal")); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(foreign, "journal", "events.jsonl"), path); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := New(store).Scan(func(Entry) error { return nil }); err == nil {
				t.Fatal("symlink source accepted")
			}
		})
	}
}
