package logship

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCancelledPrunePreservesOriginalArchiveAge(t *testing.T) {
	logs := t.TempDir()
	item := createArchive(t, logs, "old", 8*24*time.Hour)
	second := filepath.Join(item, "second")
	if err := os.WriteFile(second, []byte("remaining log"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(item, old, old); err != nil {
		t.Fatal(err)
	}
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := cancelWhenMissing{Context: base, cancel: cancel, path: filepath.Join(item, "log")}
	if err := Prune(ctx, logs, 7*24*time.Hour, 1<<30); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected interrupted prune, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(item, archiveMarker)); err != nil {
		t.Fatalf("marker missing: %v", err)
	}
	if _, err := os.Stat(second); err != nil {
		t.Fatalf("probe did not retain log: %v", err)
	}
	if err := Prune(context.Background(), logs, 7*24*time.Hour, 1<<30); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(item); !os.IsNotExist(err) {
		t.Fatalf("expired archive survived retry because partial deletion reset its mtime: %v", err)
	}
}
