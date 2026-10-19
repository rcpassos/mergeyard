package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/fault"
)

func TestRemoveTreeChecksContextBetweenEntries(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
		wait bool
		code string
	}{
		{"canceled", func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		}, false, "git.canceled"},
		{"deadline", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 10*time.Millisecond)
		}, true, "git.timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for _, name := range []string{"a", "b"} {
				if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := tc.ctx()
			defer cancel()
			readDir := func(path string) ([]os.DirEntry, error) {
				if tc.wait {
					<-ctx.Done()
				} else {
					cancel()
				}
				return os.ReadDir(path)
			}
			err := removeTreeEntries(ctx, root, readDir)
			var failure *fault.Error
			if !errors.As(err, &failure) || failure.Code != tc.code {
				t.Fatalf("remove error = %v, want %s", err, tc.code)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 2 {
				t.Fatalf("removal continued after the context ended: entries=%v err=%v", entries, err)
			}
		})
	}
}
