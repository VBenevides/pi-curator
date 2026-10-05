// Package index maintains a disposable SQLite index of the redacted journal.
package index

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
	"pi-curator/curator/internal/journal"
	"pi-curator/curator/internal/memory"
	"pi-curator/curator/internal/record"
	"pi-curator/curator/internal/search"
)

const schemaVersion = 2
const batchSize = 256

type Index struct {
	db      *sql.DB
	journal string
}
type Progress struct {
	Offset       int64 `json:"offset"`
	Added        int   `json:"added"`
	PendingBytes int64 `json:"pending_bytes"`
	Rebuilt      bool  `json:"rebuilt"`
}

func Open(store string) (*Index, error) {
	path := filepath.Join(store, "index.sqlite")
	for _, p := range []string{store, filepath.Join(store, "journal"), filepath.Join(store, "journal", "events.jsonl"), path, path + "-wal", path + "-shm", path + "-journal"} {
		info, err := os.Lstat(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("index refuses symlink %s", p)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA busy_timeout=5000; PRAGMA synchronous=FULL;
 CREATE TABLE IF NOT EXISTS checkpoint(singleton INTEGER PRIMARY KEY CHECK(singleton=1), version INTEGER NOT NULL, identity TEXT NOT NULL, offset INTEGER NOT NULL, tail_hash TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS events(id TEXT PRIMARY KEY, session TEXT NOT NULL, category TEXT NOT NULL, time TEXT NOT NULL, offset INTEGER NOT NULL, length INTEGER NOT NULL, hash TEXT NOT NULL, content TEXT NOT NULL, call_id TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS event_session ON events(session,offset);
 CREATE INDEX IF NOT EXISTS event_call ON events(session,call_id);
 CREATE TABLE IF NOT EXISTS anchors(event_id TEXT NOT NULL, kind TEXT NOT NULL, value TEXT NOT NULL, PRIMARY KEY(event_id,kind,value));
 CREATE INDEX IF NOT EXISTS anchor_value ON anchors(value,event_id);
 CREATE VIRTUAL TABLE IF NOT EXISTS event_fts USING fts5(id UNINDEXED,content, tokenize='unicode61');`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Index{db: db, journal: filepath.Join(store, "journal", "events.jsonl")}, nil
}

// Existing opens the sidecar read-only and never creates one during retrieval.
func Existing(store string) (*Index, error) {
	path := filepath.Join(store, "index.sqlite")
	for _, p := range []string{store, filepath.Join(store, "journal"), filepath.Join(store, "journal", "events.jsonl"), path, path + "-wal", path + "-shm", path + "-journal"} {
		info, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) && (p == path+"-wal" || p == path+"-shm" || p == path+"-journal") {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("index unavailable; run curator index: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("index refuses symlink %s", p)
		}
	}
	uri := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return &Index{db: db, journal: filepath.Join(store, "journal", "events.jsonl")}, nil
}
func (idx *Index) Close() error { return idx.db.Close() }
func identity(info os.FileInfo) string {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)
	}
	return info.Name() + ":" + info.ModTime().String()
}
func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func tailHash(f *os.File, offset int64) (string, error) {
	if offset < 0 {
		return "", errors.New("invalid negative index checkpoint")
	}
	start := offset - 256
	if start < 0 {
		start = 0
	}
	b := make([]byte, offset-start)
	if _, err := f.ReadAt(b, start); err != nil && err != io.EOF {
		return "", err
	}
	return digest(b), nil
}

// Update commits bounded batches and their checkpoint atomically. An incomplete
// trailing record stays pending; malformed complete records stop indexing.
func (idx *Index) Update(rebuild bool) (Progress, error) {
	f, err := journal.OpenSource(idx.journal, os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return Progress{}, nil
	}
	if err != nil {
		return Progress{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Progress{}, err
	}
	var version int
	var storedIdentity, storedHash string
	var offset int64
	err = idx.db.QueryRow("SELECT version,identity,offset,tail_hash FROM checkpoint WHERE singleton=1").Scan(&version, &storedIdentity, &offset, &storedHash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Progress{}, err
	}
	if err == nil {
		rebuild = rebuild || version != schemaVersion || storedIdentity != identity(info) || offset < 0 || offset > info.Size()
		if !rebuild {
			currentHash, herr := tailHash(f, offset)
			if herr != nil {
				return Progress{}, herr
			}
			rebuild = currentHash != storedHash
		}
	}
	progress := Progress{Rebuilt: rebuild}
	if rebuild {
		tx, err := idx.db.Begin()
		if err != nil {
			return progress, err
		}
		if _, err = tx.Exec("DELETE FROM event_fts; DELETE FROM anchors; DELETE FROM events; DELETE FROM checkpoint;"); err != nil {
			tx.Rollback()
			return progress, err
		}
		if err = tx.Commit(); err != nil {
			return progress, err
		}
		offset = 0
	}
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return progress, err
	}
	reader := bufio.NewReaderSize(io.LimitReader(f, info.Size()-offset), 64<<10)
	for {
		tx, err := idx.db.Begin()
		if err != nil {
			return progress, err
		}
		done := false
		added := 0
		next := offset
		for range batchSize {
			var line []byte
			var readErr error
			for {
				fragment, fragmentErr := reader.ReadSlice('\n')
				if len(line)+len(fragment) > journal.MaxRecordBytes+1 {
					tx.Rollback()
					return progress, journal.ErrRecordTooLarge
				}
				line = append(line, fragment...)
				if fragmentErr != bufio.ErrBufferFull {
					readErr = fragmentErr
					break
				}
			}
			if readErr == io.EOF {
				done = true
				break
			}
			if readErr != nil {
				tx.Rollback()
				return progress, readErr
			}
			_, decoded, err := record.Decode(bytes.TrimSuffix(line, []byte{'\n'}))
			if err != nil {
				tx.Rollback()
				return progress, fmt.Errorf("index offset %d: %w", next, err)
			}
			if ev, ok := decoded.(*record.Event); ok {
				result, err := tx.Exec("INSERT OR IGNORE INTO events VALUES(?,?,?,?,?,?,?,?,?)", ev.ID, ev.SessionID, ev.Category, ev.CreatedAtUTC, next, len(line), digest(line), ev.Content, ev.CallID)
				if err != nil {
					tx.Rollback()
					return progress, err
				}
				affected, err := result.RowsAffected()
				if err != nil {
					tx.Rollback()
					return progress, err
				}
				if affected > 0 {
					if _, err = tx.Exec("INSERT INTO event_fts(id,content) VALUES(?,?)", ev.ID, ev.Content); err != nil {
						tx.Rollback()
						return progress, err
					}
					for _, anchor := range anchors(ev) {
						if _, err = tx.Exec("INSERT INTO anchors VALUES(?,?,?)", ev.ID, anchor.kind, anchor.value); err != nil {
							tx.Rollback()
							return progress, err
						}
					}
					added++
				}
			}
			next += int64(len(line))
		}
		hash, err := tailHash(f, next)
		if err != nil {
			tx.Rollback()
			return progress, err
		}
		if _, err = tx.Exec("INSERT OR REPLACE INTO checkpoint VALUES(1,?,?,?,?)", schemaVersion, identity(info), next, hash); err != nil {
			tx.Rollback()
			return progress, err
		}
		if err = tx.Commit(); err != nil {
			return progress, err
		}
		offset = next
		progress.Offset = offset
		progress.Added += added
		progress.PendingBytes = info.Size() - offset
		if done {
			return progress, nil
		}
	}
}

func (idx *Index) fresh() error {
	f, err := journal.OpenSource(idx.journal, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	var version int
	var id, hash string
	var offset int64
	if err = idx.db.QueryRow("SELECT version,identity,offset,tail_hash FROM checkpoint WHERE singleton=1").Scan(&version, &id, &offset, &hash); err != nil {
		return fmt.Errorf("index unavailable; run curator index: %w", err)
	}
	if version != schemaVersion || id != identity(info) || offset < 0 || offset != info.Size() {
		return errors.New("index stale or journal incomplete; run curator index (recover an incomplete tail first)")
	}
	actual, err := tailHash(f, offset)
	if err != nil {
		return err
	}
	if actual != hash {
		return errors.New("index checkpoint differs from journal; run curator index")
	}
	return nil
}

// Read verifies the ID and bytes at the indexed location before returning them.
func (idx *Index) Read(id string) (*record.Event, error) {
	if err := idx.fresh(); err != nil {
		return nil, err
	}
	var offset int64
	var length int
	var hash string
	if err := idx.db.QueryRow("SELECT offset,length,hash FROM events WHERE id=?", id).Scan(&offset, &length, &hash); err != nil {
		return nil, fmt.Errorf("event %q: %w", id, err)
	}
	if offset < 0 || length < 1 || length > journal.MaxRecordBytes+1 {
		return nil, errors.New("invalid indexed location; rebuild index")
	}
	f, err := journal.OpenSource(idx.journal, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data := make([]byte, length)
	if _, err = f.ReadAt(data, offset); err != nil {
		return nil, err
	}
	if digest(data) != hash {
		return nil, errors.New("stored event changed; rebuild index")
	}
	_, decoded, err := record.Decode(bytes.TrimSuffix(data, []byte{'\n'}))
	if err != nil {
		return nil, err
	}
	ev, ok := decoded.(*record.Event)
	if !ok || ev.ID != id {
		return nil, errors.New("indexed event ID mismatch; rebuild index")
	}
	return ev, nil
}

// Search uses literal FTS terms, never user-authored FTS syntax or SQL.
func (idx *Index) Search(query string, limit int) (search.Result, error) {
	if err := idx.fresh(); err != nil {
		return search.Result{}, err
	}
	words := strings.Fields(query)
	if len(words) > 20 {
		words = words[:20]
	}
	quoted := make([]string, 0, len(words))
	for _, word := range words {
		quoted = append(quoted, `"`+strings.ReplaceAll(word, `"`, `""`)+`"`)
	}
	if len(quoted) == 0 {
		return search.Result{}, errors.New("empty FTS query")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	expr := strings.Join(quoted, " OR ")
	rows, err := idx.db.QueryContext(ctx, `SELECT e.id FROM event_fts JOIN events e ON e.id=event_fts.id WHERE event_fts MATCH ? AND e.category IN ('user_message','assistant_message') ORDER BY bm25(event_fts),e.offset DESC LIMIT ?`, expr, limit)
	if err != nil {
		return search.Result{}, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return search.Result{}, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return search.Result{}, err
	}
	d := &memory.Data{Events: map[string]*record.Event{}}
	for _, id := range ids {
		ev, err := idx.Read(id)
		if err != nil {
			return search.Result{}, err
		}
		d.Events[id] = ev
		d.EventOrder = append(d.EventOrder, id)
	}
	// Keep existing display/excerpt policy while FTS selects candidates.
	res, err := search.Run(d, search.Options{Terms: words, Limit: limit})
	if err != nil {
		return res, err
	}
	for n := range res.Sessions {
		var goalID string
		err = idx.db.QueryRow("SELECT id FROM events WHERE session=? AND category='user_message' ORDER BY offset LIMIT 1", res.Sessions[n].SessionID).Scan(&goalID)
		if err == nil {
			goal, readErr := idx.Read(goalID)
			if readErr != nil {
				return res, readErr
			}
			if goal.SessionID != res.Sessions[n].SessionID || goal.Category != record.CategoryUserMessage {
				return res, errors.New("indexed goal scope mismatch; rebuild index")
			}
			res.Sessions[n].Goal = goal.Content
		} else if !errors.Is(err, sql.ErrNoRows) {
			return res, err
		}
	}
	return res, nil
}

// Linked returns a bounded set of related tool-event IDs, scoped to the session.
func (idx *Index) Linked(ev *record.Event) ([]string, error) {
	ids := []string{}
	if ev.CallID == "" {
		return ids, nil
	}
	rows, err := idx.db.Query("SELECT id FROM events WHERE session=? AND call_id=? AND id!=? ORDER BY offset LIMIT 101", ev.SessionID, ev.CallID, ev.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if len(ids) > 100 {
		return nil, errors.New("tool-event links exceed 100; inspect the journal directly")
	}
	return ids, rows.Err()
}
