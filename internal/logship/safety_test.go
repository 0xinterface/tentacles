package logship

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShipDiagCancellationRemovesOnlyPartialArchive(t *testing.T) {
	slot := t.TempDir()
	logs := t.TempDir()
	writeTree(t, slot, map[string]string{"_diag/log": strings.Repeat("a", 128<<10)})
	completed, err := ShipDiag(slot, logs, "completed")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := func(_ *os.File) (diskSpace, error) {
		entries, err := os.ReadDir(logs)
		if err != nil {
			return diskSpace{}, err
		}
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), ".tentacles-diag-staging-") {
				continue
			}
			info, err := os.Stat(filepath.Join(logs, entry.Name(), "log"))
			if err == nil && info.Size() > 0 {
				cancel()
			}
		}
		return diskSpace{available: 1 << 30, total: 2 << 30}, nil
	}
	dest, err := shipDiag(ctx, slot, logs, "cancelled", probe)
	if !errors.Is(err, context.Canceled) || dest != "" {
		t.Fatalf("copy did not cancel: dest=%q err=%v", dest, err)
	}
	entries, err := os.ReadDir(logs)
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(completed) {
		t.Fatalf("cancelled archive remains or completed archive removed: %v, %v", entries, err)
	}
	got, err := os.ReadFile(filepath.Join(completed, "log"))
	if err != nil || len(got) != 128<<10 {
		t.Fatalf("completed archive damaged: len=%d err=%v", len(got), err)
	}
}

func TestShipDiagStopsAtLogFilesystemLowWatermark(t *testing.T) {
	for _, low := range []diskSpace{
		{available: 128 << 20, total: 1 << 30},
		{available: 512 << 20, total: 10 << 30},
	} {
		t.Run("low space", func(t *testing.T) {
			slot := t.TempDir()
			logs := t.TempDir()
			writeTree(t, slot, map[string]string{"_diag/log": "diagnostics"})
			probe := func(_ *os.File) (diskSpace, error) { return low, nil }
			if dest, err := shipDiag(context.Background(), slot, logs, "runner", probe); err == nil || dest != "" {
				t.Fatalf("low space accepted: dest=%q err=%v", dest, err)
			}
			entries, err := os.ReadDir(logs)
			if err != nil || len(entries) != 0 {
				t.Fatalf("partial remains: %v, %v", entries, err)
			}
		})
	}
}

func TestShipDiagRechecksSpaceWhileCopying(t *testing.T) {
	slot := t.TempDir()
	logs := t.TempDir()
	writeTree(t, slot, map[string]string{"_diag/log": strings.Repeat("a", 128<<10)})
	wroteBytes := false
	probe := func(_ *os.File) (diskSpace, error) {
		entries, err := os.ReadDir(logs)
		if err != nil {
			return diskSpace{}, err
		}
		for _, entry := range entries {
			info, err := os.Stat(filepath.Join(logs, entry.Name(), "log"))
			if err == nil && info.Size() > 0 {
				wroteBytes = true
				return diskSpace{available: 64 << 20, total: 2 << 30}, nil
			}
		}
		return diskSpace{available: 1 << 30, total: 2 << 30}, nil
	}
	if dest, err := shipDiag(context.Background(), slot, logs, "runner", probe); err == nil || dest != "" {
		t.Fatalf("copy ignored disk exhaustion: dest=%q err=%v", dest, err)
	}
	if !wroteBytes {
		t.Fatal("test did not reach partial file copying")
	}
	entries, err := os.ReadDir(logs)
	if err != nil || len(entries) != 0 {
		t.Fatalf("partial remains: %v, %v", entries, err)
	}
}

func TestShipDiagCancelledBeforeStartDoesNotCreateLogs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	logs := filepath.Join(t.TempDir(), "new-logs")
	if dest, err := ShipDiagContext(ctx, t.TempDir(), logs, "runner"); !errors.Is(err, context.Canceled) || dest != "" {
		t.Fatalf("cancelled ship: dest=%q err=%v", dest, err)
	}
	if _, err := os.Stat(logs); !os.IsNotExist(err) {
		t.Fatalf("created logs on cancelled context: %v", err)
	}
}

func TestShipDiagRejectsSlotSymlinkWithTrailingSeparator(t *testing.T) {
	out := t.TempDir()
	writeTree(t, out, map[string]string{"_diag/secret": "outside"})
	link := filepath.Join(t.TempDir(), "slot")
	if err := os.Symlink(out, link); err != nil {
		t.Fatal(err)
	}
	if dest, err := ShipDiag(link+string(os.PathSeparator), t.TempDir(), "runner"); err == nil || dest != "" {
		t.Fatalf("symlinked slot accepted: %q %v", dest, err)
	}
}

func createArchive(t *testing.T, logs, runner string, age time.Duration) string {
	t.Helper()
	slot := t.TempDir()
	writeTree(t, slot, map[string]string{"_diag/log": strings.Repeat("a", 100)})
	dest, err := ShipDiag(slot, logs, runner)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	name := filepath.Base(dest)
	rest := strings.TrimPrefix(name, archivePrefix)[len("20060102T150405Z"):]
	aged := filepath.Join(logs, archivePrefix+when.UTC().Format("20060102T150405Z")+rest)
	if err := os.Rename(dest, aged); err != nil {
		t.Fatal(err)
	}
	dest = aged
	if err := os.Chtimes(dest, when, when); err != nil {
		t.Fatal(err)
	}
	return dest
}

func TestPruneEnforcesAgeAndCombinedByteBudget(t *testing.T) {
	logs := t.TempDir()
	expired := createArchive(t, logs, "expired", 8*24*time.Hour)
	old := createArchive(t, logs, "older", 2*time.Hour)
	newest := createArchive(t, logs, "newest", time.Hour)
	// Each archive holds 100 log bytes plus its small supervisor marker.
	if err := Prune(context.Background(), logs, 7*24*time.Hour, 150); err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{expired, old} {
		if _, err := os.Stat(removed); !os.IsNotExist(err) {
			t.Fatalf("archive retained: %s: %v", removed, err)
		}
	}
	if _, err := os.Stat(newest); err != nil {
		t.Fatalf("newest archive removed: %v", err)
	}
}

func TestPruneLeavesUnrelatedAndActiveDirectories(t *testing.T) {
	logs := t.TempDir()
	active := filepath.Join(logs, ".tentacles-diag-staging-0123456789abcdef0123456789abcdef")
	unrelated := filepath.Join(logs, "manual-diagnostics")
	lookalike := filepath.Join(logs, "tentacles-diag-v1-20260901T000000Z-0123456789abcdef0123456789abcdef-slot-runner")
	for _, dir := range []string{active, unrelated, lookalike} {
		writeTree(t, dir, map[string]string{"keep": "retain this"})
		old := time.Now().Add(-30 * 24 * time.Hour)
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatal(err)
		}
	}
	createArchive(t, logs, "expired", 30*24*time.Hour)
	if err := Prune(context.Background(), logs, time.Hour, 1); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{active, unrelated, lookalike} {
		if _, err := os.Stat(filepath.Join(dir, "keep")); err != nil {
			t.Fatalf("unrelated/staging tree removed: %v", err)
		}
	}
	entries, err := os.ReadDir(logs)
	if err != nil || len(entries) != 3 {
		t.Fatalf("expired archive was not pruned: %v %v", entries, err)
	}
}

func TestPruneDoesNotFollowArchiveOrChildSymlinks(t *testing.T) {
	logs := t.TempDir()
	archive := createArchive(t, logs, "expired", 30*24*time.Hour)
	outside := t.TempDir()
	writeTree(t, outside, map[string]string{"secret": "preserve"})
	if err := os.Symlink(outside, filepath.Join(archive, "linked-directory")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(logs, "tentacles-diag-v1-20260901T000000Z-fedcba9876543210fedcba9876543210-slot-runner")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := Prune(context.Background(), logs, time.Nanosecond, 1); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(outside, "secret"))
	if err != nil || string(got) != "preserve" {
		t.Fatalf("symlink target changed: %q %v", got, err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("unowned symlink removed: %v", err)
	}
	if _, err := os.Lstat(archive); !os.IsNotExist(err) {
		t.Fatalf("owned expired archive retained: %v", err)
	}
}

func TestPruneCancellationDoesNotDeleteArchives(t *testing.T) {
	logs := t.TempDir()
	archive := createArchive(t, logs, "expired", 30*24*time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Prune(ctx, logs, time.Hour, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("prune ignored cancellation: %v", err)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("cancelled prune removed archive: %v", err)
	}
}

func TestShipDiagDirectorySwapCannotRedirectOpenDescriptor(t *testing.T) {
	slot := t.TempDir()
	logs := t.TempDir()
	outside := t.TempDir()
	writeTree(t, slot, map[string]string{"_diag/nested/log": "original diagnostics"})
	writeTree(t, outside, map[string]string{"secret": "outside secret"})
	swapped := false
	probe := func(_ *os.File) (diskSpace, error) {
		entries, err := os.ReadDir(logs)
		if err != nil {
			return diskSpace{}, err
		}
		if len(entries) != 0 && !swapped {
			// The input directory descriptor is already open when its output
			// directory is about to be created. Replace its pathname now.
			nested := filepath.Join(slot, "_diag", "nested")
			if err := os.Rename(nested, filepath.Join(slot, "old-nested")); err != nil {
				return diskSpace{}, err
			}
			if err := os.Symlink(outside, nested); err != nil {
				return diskSpace{}, err
			}
			swapped = true
		}
		return diskSpace{available: 1 << 30, total: 2 << 30}, nil
	}
	dest, err := shipDiag(context.Background(), slot, logs, "runner", probe)
	if err != nil {
		t.Fatal(err)
	}
	if !swapped {
		t.Fatal("source directory was not swapped during the copy")
	}
	got, err := os.ReadFile(filepath.Join(dest, "nested", "log"))
	if err != nil || string(got) != "original diagnostics" {
		t.Fatalf("lost anchored source: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "nested", "secret")); !os.IsNotExist(err) {
		t.Fatalf("copied data through replaced parent symlink: %v", err)
	}
}

func TestShipDiagRejectsHardlinkAddedDuringCopy(t *testing.T) {
	slot := t.TempDir()
	logs := t.TempDir()
	writeTree(t, slot, map[string]string{"_diag/log": strings.Repeat("a", 128<<10)})
	linked := false
	probe := func(_ *os.File) (diskSpace, error) {
		entries, err := os.ReadDir(logs)
		if err != nil {
			return diskSpace{}, err
		}
		for _, entry := range entries {
			info, err := os.Stat(filepath.Join(logs, entry.Name(), "log"))
			if err == nil && info.Size() > 0 && !linked {
				if err := os.Link(filepath.Join(slot, "_diag", "log"), filepath.Join(slot, "new-link")); err != nil {
					return diskSpace{}, err
				}
				linked = true
			}
		}
		return diskSpace{available: 1 << 30, total: 2 << 30}, nil
	}
	if dest, err := shipDiag(context.Background(), slot, logs, "runner", probe); err == nil || dest != "" {
		t.Fatalf("copied newly hardlinked file: dest=%q err=%v", dest, err)
	}
	if !linked {
		t.Fatal("test did not add a link during copy")
	}
	entries, err := os.ReadDir(logs)
	if err != nil || len(entries) != 0 {
		t.Fatalf("partial remains: %v %v", entries, err)
	}
}

func TestShipDiagBoundsFileGrowingDuringCopy(t *testing.T) {
	slot := t.TempDir()
	logs := t.TempDir()
	writeTree(t, slot, map[string]string{"_diag/log": "small at open time"})
	grown := false
	probe := func(_ *os.File) (diskSpace, error) {
		entries, err := os.ReadDir(logs)
		if err != nil {
			return diskSpace{}, err
		}
		for _, entry := range entries {
			if _, err := os.Stat(filepath.Join(logs, entry.Name(), "log")); err == nil && !grown {
				if err := os.Truncate(filepath.Join(slot, "_diag", "log"), 65<<20); err != nil {
					return diskSpace{}, err
				}
				grown = true
			}
		}
		return diskSpace{available: 1 << 30, total: 2 << 30}, nil
	}
	if dest, err := shipDiag(context.Background(), slot, logs, "runner", probe); err == nil || dest != "" {
		t.Fatalf("growing file exceeded copy budget: dest=%q err=%v", dest, err)
	}
	if !grown {
		t.Fatal("test did not grow the source during copy")
	}
	entries, err := os.ReadDir(logs)
	if err != nil || len(entries) != 0 {
		t.Fatalf("partial remains: %v %v", entries, err)
	}
}

func TestPruneRejectsHardlinkedOrSymlinkedMarkers(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			logs := t.TempDir()
			archive := createArchive(t, logs, "protected", time.Hour)
			marker := filepath.Join(archive, ".tentacles-archive")
			outside := filepath.Join(t.TempDir(), "marker")
			if err := os.Rename(marker, outside); err != nil {
				t.Fatal(err)
			}
			link := os.Symlink
			if kind == "hardlink" {
				link = os.Link
			}
			if err := link(outside, marker); err != nil {
				t.Fatal(err)
			}
			if err := Prune(context.Background(), logs, time.Nanosecond, 1); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(archive, "log")); err != nil {
				t.Fatalf("untrusted marker accepted: %v", err)
			}
		})
	}
}

type cancelWhenMissing struct {
	context.Context
	cancel context.CancelFunc
	path   string
}

func (c cancelWhenMissing) Err() error {
	if _, err := os.Lstat(c.path); os.IsNotExist(err) {
		c.cancel()
	}
	return c.Context.Err()
}

func TestPruneCancellationKeepsMarkerUntilDirectoryRemoval(t *testing.T) {
	logs := t.TempDir()
	archive := createArchive(t, logs, "expired", time.Hour)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := cancelWhenMissing{Context: base, cancel: cancel, path: filepath.Join(archive, ".tentacles-archive")}
	err := Prune(ctx, logs, time.Nanosecond, 1)
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(archive); os.IsNotExist(err) {
		return
	}
	// Cancellation may interrupt a prune, but every surviving archive must
	// keep its ownership marker so the next prune can reclaim its disk space.
	if _, err := os.Stat(filepath.Join(archive, ".tentacles-archive")); err != nil {
		t.Fatalf("interrupted prune left an unmarked archive: %v", err)
	}
	if err := Prune(context.Background(), logs, time.Nanosecond, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Fatalf("retry could not prune interrupted archive: %v", err)
	}
}
