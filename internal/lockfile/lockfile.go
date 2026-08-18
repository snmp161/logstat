// Package lockfile guards against a second instance running with the same
// configuration, using an advisory flock(2) on a per-instance lock file.
package lockfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// ErrLocked is returned when another process already holds the lock.
var ErrLocked = errors.New("lock file is already held by another instance")

// Lock is an acquired advisory lock.
type Lock struct {
	path string
	f    *os.File
}

// Acquire takes an exclusive non-blocking flock on path, creating the file and
// its parent directory if needed, and writes the current PID into it.
func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create lock directory for %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644) //nolint:gosec // operator-provided path
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%s: %w", path, ErrLocked)
		}
		return nil, fmt.Errorf("flock %s: %w", path, err)
	}
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return &Lock{path: path, f: f}, nil
}

// Release drops the lock and closes the file. The file itself is left in place:
// on a tmpfs RuntimeDirectory it disappears with the directory anyway, and
// removing it would race with a concurrent Acquire.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}

// Path returns the lock file path.
func (l *Lock) Path() string { return l.path }
