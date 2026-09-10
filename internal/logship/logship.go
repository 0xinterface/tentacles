// Package logship preserves bounded, private diagnostic archives before a slot
// is wiped. Source entries are job-controlled and must not be trusted.
package logship

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	maxArchiveBytes      = 64 << 20
	maxArchiveEntries    = 4096
	maxArchiveDepth      = 64
	archivePrefix        = "tentacles-diag-v1-"
	stagingPrefix        = ".tentacles-diag-staging-"
	archiveMarker        = ".tentacles-archive"
	archiveMarkerContent = "tentacles diagnostic archive v1\n"
)

// ShipDiag calls ShipDiagContext with an uncancelled context.
func ShipDiag(slotDir, logDir, runnerName string) (string, error) {
	return ShipDiagContext(context.Background(), slotDir, logDir, runnerName)
}

// ShipDiagContext copies regular, singly-linked files under slotDir/_diag.
// Symlinks and special files are skipped; a symlinked _diag root is rejected.
// Archives are limited to 64 MiB, 4096 entries and 64 directory levels. A complete
// archive becomes visible by atomic rename; errors/cancellation remove staging.
// Completed archive directories are 0700 and files are 0600. The log directory
// and its ancestors must be controlled by the supervisor, not runner jobs.
// Writes stop before free space falls below 256 MiB or 10% of the log filesystem.
func ShipDiagContext(ctx context.Context, slotDir, logDir, runnerName string) (string, error) {
	return shipDiag(ctx, slotDir, logDir, runnerName, availableSpace)
}

type diskSpace struct {
	available uint64
	total     uint64
}

func availableSpace(dir *os.File) (diskSpace, error) {
	var stat unix.Statfs_t
	if err := unix.Fstatfs(int(dir.Fd()), &stat); err != nil {
		return diskSpace{}, err
	}
	return diskSpace{available: stat.Bavail * uint64(stat.Bsize), total: stat.Blocks * uint64(stat.Bsize)}, nil
}

func shipDiag(
	ctx context.Context,
	slotDir string,
	logDir string,
	runnerName string,
	probe func(*os.File) (diskSpace, error),
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if slotDir == "" || logDir == "" {
		return "", errors.New("logship: empty slotDir or logDir")
	}
	slot, err := openDirectory(slotDir)
	if err != nil {
		return "", fmt.Errorf("logship: open slot: %w", err)
	}
	defer slot.Close()
	src, err := openChild(slot, "_diag", true)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("logship: open _diag: %w", err)
	}
	defer src.Close()

	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return "", fmt.Errorf("logship: create log directory: %w", err)
	}
	logs, err := os.OpenRoot(logDir)
	if err != nil {
		return "", fmt.Errorf("logship: open log directory: %w", err)
	}
	defer logs.Close()
	logFile, err := logs.Open(".")
	if err != nil {
		return "", fmt.Errorf("logship: open log filesystem: %w", err)
	}
	defer logFile.Close()
	checkSpace := func(bytes uint64) error {
		space, err := probe(logFile)
		if err != nil {
			return fmt.Errorf("probe log filesystem: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		reserve := max(uint64(256<<20), space.total/10)
		if space.available <= reserve || bytes > space.available-reserve {
			return errors.New("log filesystem below free-space watermark")
		}
		return nil
	}
	if err := checkSpace(4096); err != nil {
		return "", err
	}

	suffix := make([]byte, 16)
	if _, err := rand.Read(suffix); err != nil {
		return "", fmt.Errorf("logship: random archive name: %w", err)
	}
	id := hex.EncodeToString(suffix)
	staging := stagingPrefix + id
	if err := logs.Mkdir(staging, 0o700); err != nil {
		return "", fmt.Errorf("logship: create staging: %w", err)
	}
	defer logs.RemoveAll(staging)
	dest, err := logs.OpenRoot(staging)
	if err != nil {
		return "", fmt.Errorf("logship: open staging: %w", err)
	}
	defer dest.Close()

	copier := diagCopier{
		ctx:        ctx,
		dest:       dest,
		bytesLeft:  maxArchiveBytes - int64(len(archiveMarkerContent)),
		checkSpace: checkSpace,
	}
	if err := copier.copyDirectory(src, ".", 0); err != nil {
		return "", fmt.Errorf("logship: copy diagnostics: %w", err)
	}
	if err := checkSpace(uint64(len(archiveMarkerContent)) + 4096); err != nil {
		return "", err
	}
	if err := dest.WriteFile(archiveMarker, []byte(archiveMarkerContent), 0o600); err != nil {
		return "", fmt.Errorf("logship: write archive marker: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	slotID, _ := sanitize(filepath.Base(filepath.Clean(slotDir)))
	name, valid := sanitize(runnerName)
	if !valid {
		name = "unknown"
	}
	completed := archivePrefix + time.Now().UTC().Format("20060102T150405Z") + "-" + id + "-" + slotID + "-" + name
	if err := logs.Rename(staging, completed); err != nil {
		return "", fmt.Errorf("logship: publish archive: %w", err)
	}
	return filepath.Join(logDir, completed), nil
}

type diagCopier struct {
	ctx        context.Context
	dest       *os.Root
	bytesLeft  int64
	entries    int
	checkSpace func(uint64) error
}

func (c *diagCopier) copyDirectory(src *os.File, relative string, depth int) error {
	if depth > maxArchiveDepth {
		return errors.New("directory depth limit exceeded")
	}
	for {
		if err := c.ctx.Err(); err != nil {
			return err
		}
		entries, err := src.ReadDir(128)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		for _, entry := range entries {
			if err := c.ctx.Err(); err != nil {
				return err
			}
			c.entries++
			if c.entries > maxArchiveEntries {
				return errors.New("archive entry limit exceeded")
			}
			if relative == "." && entry.Name() == archiveMarker {
				continue
			}
			if err := c.copyEntry(src, relative, entry.Name(), depth); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
	}
}

func (c *diagCopier) copyEntry(parent *os.File, relative, name string, depth int) error {
	var before unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	kind := before.Mode & unix.S_IFMT
	if kind != unix.S_IFDIR && kind != unix.S_IFREG {
		return nil
	}
	if kind == unix.S_IFREG && before.Nlink != 1 {
		return nil
	}
	in, err := openChild(parent, name, kind == unix.S_IFDIR)
	if err != nil {
		return err
	}
	defer in.Close()
	var opened unix.Stat_t
	if err := unix.Fstat(int(in.Fd()), &opened); err != nil {
		return err
	}
	// Validate the opened descriptor, not merely the directory entry inspected
	// before open: the job may replace an entry at any point in the traversal.
	if opened.Mode&unix.S_IFMT != kind || before.Dev != opened.Dev || before.Ino != opened.Ino {
		return errors.New("source entry changed during open")
	}
	target := filepath.Join(relative, name)
	if kind == unix.S_IFDIR {
		if err := c.checkSpace(4096); err != nil {
			return err
		}
		if err := c.dest.Mkdir(target, 0o700); err != nil {
			return err
		}
		return c.copyDirectory(in, target, depth+1)
	}
	if opened.Nlink != 1 {
		return nil
	}
	if opened.Size > c.bytesLeft {
		return errors.New("archive byte limit exceeded")
	}
	return c.copyFile(in, target)
}

func (c *diagCopier) copyFile(in *os.File, target string) error {
	if err := c.checkSpace(4096); err != nil {
		return err
	}
	out, err := c.dest.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	buffer := make([]byte, 32<<10)
	for {
		if err := c.ctx.Err(); err != nil {
			return err
		}
		n, err := in.Read(buffer)
		if int64(n) > c.bytesLeft {
			return errors.New("archive byte limit exceeded")
		}
		if n > 0 {
			if err := c.checkSpace(uint64(n)); err != nil {
				return err
			}
			if _, writeErr := out.Write(buffer[:n]); writeErr != nil {
				return writeErr
			}
			c.bytesLeft -= int64(n)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	var after unix.Stat_t
	if err := unix.Fstat(int(in.Fd()), &after); err != nil {
		return err
	}
	if after.Nlink != 1 {
		return errors.New("source file acquired a hardlink during copy")
	}
	return out.Close()
}

func openDirectory(path string) (*os.File, error) {
	return os.OpenFile(filepath.Clean(path), os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
}

// Every child is a single name under an already-open parent. Openat deliberately
// avoids os.Root.OpenFile, which resolves symlinks even with O_NOFOLLOW supplied.
func openChild(parent *os.File, name string, directory bool) (*os.File, error) {
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
	if directory {
		flags |= unix.O_DIRECTORY
	}
	fd, err := unix.Openat(int(parent.Fd()), name, flags, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func sanitize(s string) (string, bool) {
	var b strings.Builder
	valid := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
			valid = true
		default:
			b.WriteByte('_')
		}
	}
	return b.String(), valid
}
