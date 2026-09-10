package logship

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

var archiveNamePattern = regexp.MustCompile(`^tentacles-diag-v1-[0-9]{8}T[0-9]{6}Z-[a-f0-9]{32}-[A-Za-z0-9._-]+$`)

type archive struct {
	name    string
	created time.Time
	bytes   int64
	device  uint64
	inode   uint64
}

// Prune removes completed archives past maxAge, then removes oldest archives
// until their combined size fits maxBytes. Nonpositive limits are rejected.
// Only strictly named, supervisor-owned archives with a valid private marker
// are eligible; unrelated directories and active staging are left alone.
// Callers serialize pruning with shipping. The log directory and its ancestors
// must be controlled by the supervisor.
func Prune(ctx context.Context, logDir string, maxAge time.Duration, maxBytes int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if logDir == "" || maxAge <= 0 || maxBytes <= 0 {
		return errors.New("logship: retention requires a log directory and positive limits")
	}
	logs, err := openDirectory(logDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("logship: open retention directory: %w", err)
	}
	defer logs.Close()
	archives, err := findArchives(ctx, logs)
	if err != nil {
		return fmt.Errorf("logship: inventory archives: %w", err)
	}
	slices.SortFunc(archives, func(a, b archive) int {
		return a.created.Compare(b.created)
	})
	var total int64
	for _, archive := range archives {
		if archive.bytes > math.MaxInt64-total {
			return errors.New("logship: archive size overflow")
		}
		total += archive.bytes
	}
	cutoff := time.Now().Add(-maxAge)
	for _, archive := range archives {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !archive.created.Before(cutoff) && total <= maxBytes {
			continue
		}
		var current unix.Stat_t
		if err := unix.Fstatat(int(logs.Fd()), archive.name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("logship: recheck archive: %w", err)
		}
		if uint64(current.Dev) != archive.device || uint64(current.Ino) != archive.inode {
			return errors.New("logship: archive changed during pruning")
		}
		if err := removeTree(ctx, logs, archive.name, true); err != nil {
			return fmt.Errorf("logship: prune %s: %w", archive.name, err)
		}
		total -= archive.bytes
	}
	return nil
}

func findArchives(ctx context.Context, logs *os.File) ([]archive, error) {
	archives := []archive{}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries, err := logs.ReadDir(128)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !entry.IsDir() || !archiveNamePattern.MatchString(entry.Name()) {
				continue
			}
			item, owned, err := inspectArchive(ctx, logs, entry.Name())
			if err != nil {
				return nil, err
			}
			if owned {
				archives = append(archives, item)
			}
		}
		if errors.Is(err, io.EOF) {
			return archives, nil
		}
	}
}

func inspectArchive(ctx context.Context, logs *os.File, name string) (archive, bool, error) {
	dir, err := openChild(logs, name, true)
	if err != nil {
		return archive{}, false, err
	}
	defer dir.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(int(dir.Fd()), &stat); err != nil {
		return archive{}, false, err
	}
	if stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o077 != 0 {
		return archive{}, false, nil
	}
	owned, err := validMarker(dir)
	if err != nil || !owned {
		return archive{}, false, err
	}
	// Directory mtime changes during partial deletion. The validated name
	// retains original creation time across cancellation and retry.
	stamp := strings.TrimPrefix(name, archivePrefix)[:len("20060102T150405Z")]
	created, err := time.Parse("20060102T150405Z", stamp)
	if err != nil {
		return archive{}, false, err
	}
	size, err := directoryBytes(ctx, dir, 0)
	if err != nil {
		return archive{}, false, err
	}
	return archive{
		name:    name,
		created: created,
		bytes:   size,
		device:  uint64(stat.Dev),
		inode:   uint64(stat.Ino),
	}, true, nil
}

func validMarker(dir *os.File) (bool, error) {
	var before unix.Stat_t
	if err := unix.Fstatat(int(dir.Fd()), archiveMarker, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG || before.Nlink != 1 {
		return false, nil
	}
	marker, err := openChild(dir, archiveMarker, false)
	if err != nil {
		return false, err
	}
	defer marker.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(int(marker.Fd()), &stat); err != nil {
		return false, err
	}
	isPrivateRegular := stat.Mode&unix.S_IFMT == unix.S_IFREG && stat.Mode&0o077 == 0
	if !isPrivateRegular || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		return false, nil
	}
	if before.Dev != stat.Dev || before.Ino != stat.Ino {
		return false, errors.New("archive marker changed during open")
	}
	contents, err := io.ReadAll(io.LimitReader(marker, int64(len(archiveMarkerContent))+1))
	return string(contents) == archiveMarkerContent, err
}

func directoryBytes(ctx context.Context, dir *os.File, depth int) (int64, error) {
	if depth > maxArchiveDepth {
		return 0, errors.New("archive directory depth limit exceeded")
	}
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		entries, err := dir.ReadDir(128)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			var stat unix.Stat_t
			if err := unix.Fstatat(int(dir.Fd()), entry.Name(), &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return 0, err
			}
			var bytes int64
			switch stat.Mode & unix.S_IFMT {
			case unix.S_IFREG:
				bytes = stat.Size
			case unix.S_IFDIR:
				child, err := openChild(dir, entry.Name(), true)
				if err != nil {
					return 0, err
				}
				bytes, err = directoryBytes(ctx, child, depth+1)
				child.Close()
				if err != nil {
					return 0, err
				}
			}
			if bytes < 0 || bytes > math.MaxInt64-size {
				return 0, errors.New("archive size overflow")
			}
			size += bytes
		}
		if errors.Is(err, io.EOF) {
			return size, nil
		}
	}
}

func removeTree(ctx context.Context, parent *os.File, name string, keepMarker bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return unix.Unlinkat(int(parent.Fd()), name, 0)
	}
	dir, err := openChild(parent, name, true)
	if err != nil {
		return err
	}
	defer dir.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := dir.ReadDir(128)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		for _, entry := range entries {
			if keepMarker && entry.Name() == archiveMarker {
				continue
			}
			if err := removeTree(ctx, dir, entry.Name(), false); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Keep ownership discoverable if cancellation interrupts removal. Once
	// only the marker remains, finish the two unlinks without a cancellation
	// gap that could strand a directory no later Prune would recognize.
	if keepMarker {
		if err := unix.Unlinkat(int(dir.Fd()), archiveMarker, 0); err != nil {
			return err
		}
	}
	return unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR)
}
