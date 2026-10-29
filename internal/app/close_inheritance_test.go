package app

import (
	"errors"
	"syscall"
	"testing"
)

// A concurrent fork can temporarily retain a duplicate of the lock's open
// file description before exec closes it. Close must release ownership even
// while that inherited descriptor still exists.
func TestCloseReleasesOwnershipWithInheritedDescriptor(t *testing.T) {
	root := t.TempDir()
	first, err := Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.Close() })
	inherited, err := syscall.Dup(int(first.lock.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(inherited)
	if second, err := Open(t.Context(), root); !errors.Is(err, ErrWorkspaceLocked) {
		if second != nil {
			second.Close()
		}
		t.Fatalf("live owner not protected: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(t.Context(), root)
	if err != nil {
		t.Fatalf("closed owner retained its lock through inherited descriptor: %v", err)
	}
	defer second.Close()
}
