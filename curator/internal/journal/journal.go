// Package journal implements the authoritative append-only JSONL journal.
//
// Flush policy: Append writes every record of a batch in one write call and
// fsyncs the file (and the journal directory when the file is created) before
// returning, so a nil error means the records are durable. Scans stream the
// file line by line and never hold more than one record in memory.
package journal

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"pi-curator/curator/internal/lock"
	"pi-curator/curator/internal/record"
)

// Resource limits, defined up front (NFR-001, NFR-004).
const (
	// MaxRecordBytes bounds one encoded record, excluding the newline.
	MaxRecordBytes = 1 << 20
	// MaxProblems bounds how many malformed records one scan reports.
	MaxProblems = 100
)

// ErrIncompleteTail is returned by Append while an interrupted partial record
// ends the journal; Recover quarantines it first.
var ErrIncompleteTail = errors.New("journal ends with an incomplete record")

// ErrRecordTooLarge reports a record over MaxRecordBytes. It is never
// truncated or dropped silently: the caller must surface it.
var ErrRecordTooLarge = errors.New("record exceeds size limit")

// Journal is the authoritative journal file of one store.
type Journal struct{ path string }

// New returns the journal under storeDir (<root>/.curator).
func New(storeDir string) *Journal {
	return &Journal{path: filepath.Join(storeDir, "journal", "events.jsonl")}
}

// Path is the journal file path.
func (j *Journal) Path() string { return j.path }

// Entry is one valid record delivered by Scan.
type Entry struct {
	Line   int
	Offset int64
	Kind   record.Kind
	Rec    any
}

// Problem describes a malformed record or an incomplete tail.
type Problem struct {
	Line   int
	Offset int64
	Bytes  int
	Err    error
}

func (p Problem) String() string {
	return fmt.Sprintf("line %d offset %d (%d bytes): %v", p.Line, p.Offset, p.Bytes, p.Err)
}

// Report summarizes a scan.
type Report struct {
	Records int
	// Malformed lists invalid interior records (up to MaxProblems). They are
	// never delivered to the scan callback as evidence.
	Malformed        []Problem
	MalformedOverrun bool
	// Tail is the incomplete trailing record, if any.
	Tail *Problem
}

// Healthy is true when no malformed record or incomplete tail was seen.
func (r Report) Healthy() bool { return len(r.Malformed) == 0 && r.Tail == nil && !r.MalformedOverrun }

// Scan streams every valid record to fn in order. A missing journal is empty.
// An error from fn stops the scan and is returned.
func (j *Journal) Scan(fn func(Entry) error) (Report, error) {
	f, err := OpenSource(j.path, os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return Report{}, nil
	}
	if err != nil {
		return Report{}, fmt.Errorf("open journal: %w", err)
	}
	defer f.Close()

	var rep Report
	r := bufio.NewReaderSize(f, 64<<10)
	var offset int64
	for line := 1; ; line++ {
		data, consumed, complete, tooLong, err := readLine(r)
		if err != nil {
			return rep, fmt.Errorf("read journal line %d: %w", line, err)
		}
		if consumed == 0 {
			return rep, nil
		}
		size := int(consumed)
		switch {
		case !complete:
			rep.Tail = &Problem{Line: line, Offset: offset, Bytes: size, Err: ErrIncompleteTail}
			return rep, nil
		case tooLong:
			j.problem(&rep, Problem{Line: line, Offset: offset, Bytes: size, Err: ErrRecordTooLarge})
		default:
			kind, rec, derr := record.Decode(data)
			if derr != nil {
				j.problem(&rep, Problem{Line: line, Offset: offset, Bytes: size, Err: derr})
				break
			}
			rep.Records++
			if err := fn(Entry{Line: line, Offset: offset, Kind: kind, Rec: rec}); err != nil {
				return rep, err
			}
		}
		offset += int64(size)
	}
}

func (j *Journal) problem(rep *Report, p Problem) {
	if len(rep.Malformed) < MaxProblems {
		rep.Malformed = append(rep.Malformed, p)
		return
	}
	rep.MalformedOverrun = true
}

// readLine reads one line without the newline and reports the bytes consumed.
// When the line exceeds MaxRecordBytes its remainder is consumed and discarded
// (tooLong, data nil). complete is false when EOF arrived before a newline.
func readLine(r *bufio.Reader) (data []byte, consumed int64, complete, tooLong bool, err error) {
	var buf []byte
	for {
		chunk, rerr := r.ReadSlice('\n')
		consumed += int64(len(chunk))
		if !tooLong {
			if len(buf)+len(chunk) > MaxRecordBytes+1 {
				tooLong, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		switch {
		case rerr == nil:
			if tooLong {
				return nil, consumed, true, true, nil
			}
			return bytes.TrimSuffix(buf, []byte("\n")), consumed, true, false, nil
		case errors.Is(rerr, bufio.ErrBufferFull):
			continue
		case errors.Is(rerr, io.EOF):
			if tooLong {
				return nil, consumed, false, true, nil
			}
			return buf, consumed, false, false, nil
		default:
			return nil, consumed, false, false, rerr
		}
	}
}

// checkTail reports an incomplete trailing record without scanning the file.
func (j *Journal) tailIncomplete() (bool, int64, error) {
	f, err := OpenSource(j.path, os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, fmt.Errorf("open journal: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return false, 0, err
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, st.Size()-1); err != nil {
		return false, 0, fmt.Errorf("read journal tail: %w", err)
	}
	return last[0] != '\n', st.Size(), nil
}

// Append durably appends the records as one batch. The *lock.Held argument
// proves the caller owns the exclusive memory lock. Records are validated and
// size-checked before any byte is written. On a failed write the file is
// truncated back to its prior length so no partial batch remains.
func (j *Journal) Append(_ *lock.Held, recs ...interface{ Validate() error }) error {
	var batch bytes.Buffer
	for _, rec := range recs {
		line, err := record.Encode(rec)
		if err != nil {
			return err
		}
		if len(line) > MaxRecordBytes {
			return fmt.Errorf("%w: %d bytes, limit %d", ErrRecordTooLarge, len(line), MaxRecordBytes)
		}
		batch.Write(line)
		batch.WriteByte('\n')
	}
	if batch.Len() == 0 {
		return nil
	}
	if bad, _, err := j.tailIncomplete(); err != nil {
		return err
	} else if bad {
		return ErrIncompleteTail
	}

	dir := filepath.Dir(j.path)
	created := false
	if err := os.Mkdir(dir, 0o700); err == nil {
		created = true
	} else if !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create journal directory: %w", err)
	}
	f, err := OpenSource(j.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open journal for append: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("stat journal: %w", err)
	}
	prior := st.Size()
	if _, err := f.Write(batch.Bytes()); err != nil {
		return errors.Join(fmt.Errorf("append to journal: %w", err), rollback(f, prior))
	}
	if err := f.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync journal: %w", err), rollback(f, prior))
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close journal: %w", err)
	}
	if created || prior == 0 {
		return syncDir(dir)
	}
	return nil
}

// rollback truncates the journal to prior and closes it.
func rollback(f *os.File, prior int64) error {
	terr := f.Truncate(prior)
	cerr := f.Close()
	if terr != nil {
		return fmt.Errorf("roll back partial append: %w", terr)
	}
	return cerr
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open directory for sync: %w", err)
	}
	serr := d.Sync()
	cerr := d.Close()
	if serr != nil {
		return fmt.Errorf("sync directory: %w", serr)
	}
	return cerr
}

// Recover moves an incomplete trailing record into a quarantine file next to
// the journal and truncates the journal to its last complete record. It
// returns the quarantine path, or "" when there was nothing to recover.
func (j *Journal) Recover(_ *lock.Held) (string, error) {
	bad, size, err := j.tailIncomplete()
	if err != nil || !bad {
		return "", err
	}
	f, err := OpenSource(j.path, os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("open journal for recovery: %w", err)
	}
	defer f.Close()
	// Find the start of the partial record by reading backwards for '\n'.
	start := size
	chunk := make([]byte, 4096)
	for start > 0 {
		n := int64(len(chunk))
		if start < n {
			n = start
		}
		if _, err := f.ReadAt(chunk[:n], start-n); err != nil {
			return "", fmt.Errorf("scan journal tail: %w", err)
		}
		if i := bytes.LastIndexByte(chunk[:n], '\n'); i >= 0 {
			start = start - n + int64(i) + 1
			break
		}
		start -= n
	}
	partial := make([]byte, size-start)
	if _, err := f.ReadAt(partial, start); err != nil {
		return "", fmt.Errorf("read partial record: %w", err)
	}
	qpath := fmt.Sprintf("%s.tail-%d", j.path, start)
	q, err := os.OpenFile(qpath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("quarantine partial record: %w", err)
	}
	if _, err = q.Write(partial); err == nil {
		err = q.Sync()
	}
	closeErr := q.Close()
	if err != nil {
		return "", fmt.Errorf("persist quarantine: %w", err)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close quarantine: %w", closeErr)
	}
	dir, err := os.Open(filepath.Dir(qpath))
	if err != nil {
		return "", fmt.Errorf("open quarantine directory: %w", err)
	}
	err = dir.Sync()
	closeErr = dir.Close()
	if err != nil {
		return "", fmt.Errorf("persist quarantine directory: %w", err)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close quarantine directory: %w", closeErr)
	}
	if err := f.Truncate(start); err != nil {
		return "", fmt.Errorf("truncate journal: %w", err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("sync journal: %w", err)
	}
	return qpath, nil
}
