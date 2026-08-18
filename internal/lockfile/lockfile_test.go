package lockfile

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestAcquireCreatesDirectoryAndWritesPID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "inst", "logstat.lock")

	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = lock.Release() }()

	if lock.Path() != path {
		t.Errorf("Path = %q, want %q", lock.Path(), path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("lock file content %q: %v", data, err)
	}
	if pid != os.Getpid() {
		t.Errorf("pid in lock file = %d, want %d", pid, os.Getpid())
	}
}

func TestSecondAcquireIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logstat.lock")

	first, err := Acquire(path)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	if _, err := Acquire(path); !errors.Is(err, ErrLocked) {
		_ = first.Release()
		t.Fatalf("second Acquire = %v, want ErrLocked", err)
	}

	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// After the release the lock is free again.
	second, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestDifferentInstancesUseDifferentLockFiles(t *testing.T) {
	dir := t.TempDir()
	a, err := Acquire(filepath.Join(dir, "app1", "logstat.lock"))
	if err != nil {
		t.Fatalf("app1: %v", err)
	}
	defer func() { _ = a.Release() }()
	b, err := Acquire(filepath.Join(dir, "nginx", "logstat.lock"))
	if err != nil {
		t.Fatalf("nginx: %v", err)
	}
	defer func() { _ = b.Release() }()
}

func TestAcquireFailsOnUnusablePath(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "notadir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(filepath.Join(blocker, "logstat.lock")); err == nil {
		t.Fatal("expected an error when the parent path is a regular file")
	}
}

func TestReleaseOfNilAndDoubleRelease(t *testing.T) {
	var nilLock *Lock
	if err := nilLock.Release(); err != nil {
		t.Errorf("Release on nil = %v, want nil", err)
	}
	lock, err := Acquire(filepath.Join(t.TempDir(), "logstat.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Errorf("second Release = %v, want nil", err)
	}
}
