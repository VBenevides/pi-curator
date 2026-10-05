// Package lock provides the repository-scoped exclusive mutation lock held on
// <git-root>/.curator/memory.jsonl.lock by every curator mutation path.
package lock

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gofrs/flock"
)

// FileName is the lock file name inside the store directory.
const FileName = "memory.jsonl.lock"

// DefaultTimeout bounds lock acquisition when the caller sets none.
const DefaultTimeout = 5 * time.Second

// retryDelay is the positive polling interval for bounded acquisition.
const retryDelay = 20 * time.Millisecond

// ErrTimeout reports that the lock could not be acquired before the deadline.
// No memory state has been read or changed when it is returned.
var ErrTimeout = errors.New("timed out acquiring memory lock")

// Held proves the exclusive lock is held. Helpers that need the lock take a
// *Held instead of acquiring it again, so acquisition is never nested.
type Held struct {
	fl       *flock.Flock
	released bool
}

// Acquire takes the exclusive OS-managed lock, waiting at most timeout. It
// never creates the store directory and never blocks indefinitely.
func Acquire(path string, timeout time.Duration) (*Held, error) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	fl := flock.New(path)
	ok, err := fl.TryLockContext(ctx, retryDelay)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w (%s, %s)", ErrTimeout, path, timeout)
		}
		return nil, fmt.Errorf("acquire memory lock %s: %w", path, err)
	}
	if !ok {
		return nil, fmt.Errorf("%w (%s, %s)", ErrTimeout, path, timeout)
	}
	return &Held{fl: fl}, nil
}

// Release unlocks and closes the lock file without deleting it. Releasing
// twice is a no-op.
func (h *Held) Release() error {
	if h.released {
		return nil
	}
	h.released = true
	if err := h.fl.Unlock(); err != nil {
		return fmt.Errorf("release memory lock %s: %w", h.fl.Path(), err)
	}
	return nil
}

// With acquires the lock, runs fn, and always releases it, joining the unlock
// error with fn's error so neither is lost.
func With(path string, timeout time.Duration, fn func(*Held) error) error {
	h, err := Acquire(path, timeout)
	if err != nil {
		return err
	}
	return errors.Join(fn(h), h.Release())
}
