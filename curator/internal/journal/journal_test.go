package journal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pi-curator/curator/internal/lock"
	"pi-curator/curator/internal/record"
)

const ts = "2026-09-18T14:22:01Z"

func ev(id, content string) record.Event {
	return record.Event{Schema: record.SchemaEvent, ID: id, CreatedAtUTC: ts, SessionID: "s1",
		Category: record.CategoryUserMessage, Content: content, PolicyVersion: record.PolicyVersion}
}

func setup(t *testing.T) (*Journal, *lock.Held) {
	t.Helper()
	store := t.TempDir()
	h, err := lock.Acquire(filepath.Join(store, lock.FileName), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Release() })
	return New(store), h
}

func ids(t *testing.T, j *Journal) ([]string, Report) {
	t.Helper()
	var got []string
	rep, err := j.Scan(func(e Entry) error {
		got = append(got, e.Rec.(*record.Event).ID)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got, rep
}

func TestAppendThenScanReturnsRecordsInOrder(t *testing.T) {
	j, h := setup(t)
	if err := j.Append(h, ev("e1", "a"), ev("e2", "b")); err != nil {
		t.Fatal(err)
	}
	if err := j.Append(h, ev("e3", "c")); err != nil {
		t.Fatal(err)
	}
	got, rep := ids(t, j)
	if strings.Join(got, ",") != "e1,e2,e3" || !rep.Healthy() || rep.Records != 3 {
		t.Fatalf("got %v, report %+v", got, rep)
	}
	if st, _ := os.Stat(j.Path()); st.Mode().Perm() != 0o600 {
		t.Fatalf("journal mode %v, want 0600", st.Mode().Perm())
	}
}

func TestInvalidRecordInBatchWritesNothing(t *testing.T) {
	j, h := setup(t)
	bad := ev("e2", "b")
	bad.CreatedAtUTC = "yesterday"
	if err := j.Append(h, ev("e1", "a"), bad); err == nil {
		t.Fatal("invalid batch accepted")
	}
	if _, err := os.Stat(j.Path()); !os.IsNotExist(err) {
		t.Fatalf("journal written for rejected batch: %v", err)
	}
}

func TestOversizedRecordRejectedNotTruncated(t *testing.T) {
	j, h := setup(t)
	err := j.Append(h, ev("e1", strings.Repeat("x", MaxRecordBytes)))
	if !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("err = %v, want ErrRecordTooLarge", err)
	}
	if _, err := os.Stat(j.Path()); !os.IsNotExist(err) {
		t.Fatal("oversized record left bytes behind")
	}
}

func TestIncompleteTailReportedBlocksAppendAndRecoverQuarantines(t *testing.T) {
	j, h := setup(t)
	if err := j.Append(h, ev("e1", "a"), ev("e2", "b")); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(j.Path(), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"schema":"curator.event.v2","id":"e3","crea`)
	f.Close()

	got, rep := ids(t, j)
	if len(got) != 2 || rep.Tail == nil || rep.Healthy() {
		t.Fatalf("completed records must stay usable and the tail reported: %v %+v", got, rep)
	}
	if err := j.Append(h, ev("e4", "d")); !errors.Is(err, ErrIncompleteTail) {
		t.Fatalf("append over incomplete tail: %v", err)
	}
	q, err := j.Recover(h)
	if err != nil || q == "" {
		t.Fatalf("recover = %q, %v", q, err)
	}
	if data, _ := os.ReadFile(q); !strings.HasPrefix(string(data), `{"schema"`) {
		t.Fatalf("quarantine lost the partial record: %q", data)
	}
	if err := j.Append(h, ev("e4", "d")); err != nil {
		t.Fatal(err)
	}
	got, rep = ids(t, j)
	if strings.Join(got, ",") != "e1,e2,e4" || !rep.Healthy() {
		t.Fatalf("after recovery: %v %+v", got, rep)
	}
	if q2, err := j.Recover(h); err != nil || q2 != "" {
		t.Fatalf("second recover = %q, %v; want no-op", q2, err)
	}
}

func TestMalformedInteriorRecordsAreReportedNotTrusted(t *testing.T) {
	j, h := setup(t)
	if err := j.Append(h, ev("e1", "a")); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(j.Path(), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("not json\n")
	f.WriteString(strings.Repeat("y", MaxRecordBytes+10) + "\n")
	f.Close()
	if err := j.Append(h, ev("e2", "b")); err != nil {
		t.Fatal(err)
	}
	got, rep := ids(t, j)
	if strings.Join(got, ",") != "e1,e2" {
		t.Fatalf("got %v", got)
	}
	if len(rep.Malformed) != 2 || rep.Malformed[0].Line != 2 || rep.Malformed[1].Line != 3 ||
		!errors.Is(rep.Malformed[1].Err, ErrRecordTooLarge) || rep.Tail != nil {
		t.Fatalf("report %+v", rep)
	}
	// Offsets point at the real start of each bad line.
	data, _ := os.ReadFile(j.Path())
	if !strings.HasPrefix(string(data[rep.Malformed[0].Offset:]), "not json") {
		t.Fatal("malformed offset does not locate the record")
	}
	if !strings.HasPrefix(string(data[rep.Malformed[1].Offset:]), "yyyy") {
		t.Fatal("oversized offset does not locate the record")
	}
}

func TestMalformedReportIsBounded(t *testing.T) {
	j, h := setup(t)
	if err := j.Append(h, ev("e1", "a")); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(j.Path(), os.O_APPEND|os.O_WRONLY, 0)
	for i := range MaxProblems + 5 {
		fmt.Fprintf(f, "junk %d\n", i)
	}
	f.Close()
	_, rep := ids(t, j)
	if len(rep.Malformed) != MaxProblems || !rep.MalformedOverrun {
		t.Fatalf("malformed=%d overrun=%v", len(rep.Malformed), rep.MalformedOverrun)
	}
}

func TestWriteFailureIsReportedAndLeavesJournalUnchanged(t *testing.T) {
	j, h := setup(t)
	if err := j.Append(h, ev("e1", "a")); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(j.Path())
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if err := os.Chmod(j.Path(), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := j.Append(h, ev("e2", "b")); err == nil {
		t.Fatal("append to read-only journal reported success")
	}
	os.Chmod(j.Path(), 0o600)
	got, rep := ids(t, j)
	if strings.Join(got, ",") != "e1" || !rep.Healthy() {
		t.Fatalf("journal changed after failure: %v %+v", got, rep)
	}
}
