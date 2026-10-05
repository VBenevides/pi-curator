package index

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNegativeCheckpointRejectsReadAndRebuilds(t *testing.T) {
	store := t.TempDir()
	if err := os.Mkdir(filepath.Join(store, "journal"), 0700); err != nil {
		t.Fatal(err)
	}
	appendEvent(t, filepath.Join(store, "journal", "events.jsonl"), "e1", "atomic decision")
	idx, err := Open(store)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if _, err = idx.Update(false); err != nil {
		t.Fatal(err)
	}
	if _, err = idx.db.Exec("UPDATE checkpoint SET offset=-1"); err != nil {
		t.Fatal(err)
	}
	if _, err = idx.Read("e1"); err == nil {
		t.Fatal("negative checkpoint accepted")
	}
	p, err := idx.Update(false)
	if err != nil || !p.Rebuilt || p.Added != 1 {
		t.Fatalf("rebuild: %+v %v", p, err)
	}
}

func TestGoalComesFromVerifiedSource(t *testing.T) {
	store := t.TempDir()
	if err := os.Mkdir(filepath.Join(store, "journal"), 0700); err != nil {
		t.Fatal(err)
	}
	appendEvent(t, filepath.Join(store, "journal", "events.jsonl"), "goal", "original goal")
	appendEvent(t, filepath.Join(store, "journal", "events.jsonl"), "hit", "atomic decision")
	idx, err := Open(store)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if _, err = idx.Update(false); err != nil {
		t.Fatal(err)
	}
	if _, err = idx.db.Exec("UPDATE events SET content='INJECTED POLICY' WHERE id='goal'"); err != nil {
		t.Fatal(err)
	}
	res, err := idx.Search("atomic", 5)
	if err != nil || len(res.Sessions) != 1 || res.Sessions[0].Goal != "original goal" {
		t.Fatalf("source goal: %+v %v", res, err)
	}
	if _, err = idx.db.Exec("UPDATE events SET session='foreign' WHERE id='hit'"); err != nil {
		t.Fatal(err)
	}
	res, err = idx.Search("atomic", 5)
	if err != nil || res.Sessions[0].SessionID != "s" {
		t.Fatalf("source scope: %+v %v", res, err)
	}
}

func TestSQLiteAuxiliarySymlinksRejected(t *testing.T) {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		t.Run(suffix, func(t *testing.T) {
			store := t.TempDir()
			if err := os.Mkdir(filepath.Join(store, "journal"), 0700); err != nil {
				t.Fatal(err)
			}
			appendEvent(t, filepath.Join(store, "journal", "events.jsonl"), "e", "known")
			target := filepath.Join(t.TempDir(), "foreign")
			if err := os.WriteFile(target, []byte("unchanged"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(store, "index.sqlite")+suffix); err != nil {
				t.Fatal(err)
			}
			if idx, err := Open(store); err == nil {
				idx.Close()
				t.Fatal("auxiliary symlink accepted")
			}
		})
	}
}
