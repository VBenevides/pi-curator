package lock

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

const helperEnv = "CURATOR_LOCK_HELPER"

// TestMain lets the test binary act as a separate lock-holding process.
func TestMain(m *testing.M) {
	if path := os.Getenv(helperEnv); path != "" {
		if _, err := Acquire(path, time.Second); err != nil {
			os.Stderr.WriteString(err.Error() + "\n")
			os.Exit(3)
		}
		os.Stdout.WriteString("locked\n")
		select {} // hold until killed
	}
	os.Exit(m.Run())
}

func startHolder(t *testing.T, path string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), helperEnv+"="+path)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || line != "locked\n" {
		t.Fatalf("holder did not lock: %q, %v", line, err)
	}
	return cmd
}

func lockPath(t *testing.T) string { return filepath.Join(t.TempDir(), FileName) }

func TestSecondProcessTimesOutWithoutRunningMutation(t *testing.T) {
	path := lockPath(t)
	startHolder(t, path)
	ran := false
	start := time.Now()
	err := With(path, 150*time.Millisecond, func(*Held) error { ran = true; return nil })
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if ran {
		t.Fatal("mutation ran without the lock")
	}
	if d := time.Since(start); d < 100*time.Millisecond || d > 2*time.Second {
		t.Fatalf("wait took %s, want bounded near timeout", d)
	}
}

func TestLockReacquiredAfterOwnerKilled(t *testing.T) {
	path := lockPath(t)
	holder := startHolder(t, path)
	if err := holder.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = holder.Wait()
	if err := With(path, 2*time.Second, func(*Held) error { return nil }); err != nil {
		t.Fatalf("reacquire after kill: %v", err)
	}
}

func TestReleaseOnErrorKeepsLockFileIdentity(t *testing.T) {
	path := lockPath(t)
	sentinel := errors.New("write failed")
	var before, after os.FileInfo

	err := With(path, time.Second, func(*Held) error {
		before, _ = os.Stat(path)
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want fn error", err)
	}
	after, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatalf("lock file removed: %v", statErr)
	}
	if !os.SameFile(before, after) {
		t.Fatal("lock file identity changed")
	}
	// Failure released ownership; a normal acquisition and a second round
	// keep the same file.
	for range 2 {
		if err := With(path, time.Second, func(*Held) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	final, _ := os.Stat(path)
	if !os.SameFile(before, final) {
		t.Fatal("lock file replaced after reacquisition")
	}
}

func TestMissingStoreDirectoryIsNotCreated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".curator")
	if _, err := Acquire(filepath.Join(dir, FileName), time.Second); err == nil || errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want open failure", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("store dir created: %v", err)
	}
}

func TestConcurrentMutationsAreSerialized(t *testing.T) {
	path := lockPath(t)
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		inside int
		maxIn  int
		total  int
	)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := With(path, 10*time.Second, func(*Held) error {
				mu.Lock()
				inside++
				maxIn = max(maxIn, inside)
				mu.Unlock()
				time.Sleep(5 * time.Millisecond)
				mu.Lock()
				inside--
				total++
				mu.Unlock()
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if maxIn != 1 || total != 8 {
		t.Fatalf("max concurrent = %d, completed = %d", maxIn, total)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	h, err := Acquire(lockPath(t), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	if err := h.Release(); err != nil {
		t.Fatalf("second release: %v", err)
	}
}
