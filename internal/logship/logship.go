// Package logship copies a slot's _diag directory (runner diagnostics
// produced during a job) to the host log directory before the slot is
// wiped, so failed-job diagnostics survive the ephemeral runner lifecycle.
package logship

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ShipDiag copies slotDir/_diag to
// logDir/<slotID>-<UTC yyyyMMdd-HHmmss>-<runnerName> and returns the
// destination directory. It returns "", nil when the slot has no _diag
// directory (nothing to ship). The runner name is sanitized to
// [A-Za-z0-9._-]; an empty or fully-invalid name becomes "unknown".
// Symlinks inside _diag are logged and skipped, never followed.
func ShipDiag(slotDir, logDir, runnerName string) (string, error) {
	if slotDir == "" || logDir == "" {
		return "", errors.New("logship: empty slotDir or logDir")
	}
	src := filepath.Join(slotDir, "_diag")
	info, err := os.Stat(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("logship: stat %s: %w", src, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("logship: %s is not a directory", src)
	}

	slotID := filepath.Base(filepath.Clean(slotDir))
	if slotID == "/" || slotID == "." || slotID == "" {
		return "", fmt.Errorf("logship: invalid slot dir %q", slotDir)
	}
	name, ok := sanitize(runnerName)
	if !ok {
		name = "unknown"
	}

	dest := filepath.Join(logDir, slotID+"-"+time.Now().UTC().Format("20060102-150405")+"-"+name)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", fmt.Errorf("logship: mkdir %s: %w", dest, err)
	}

	if err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			slog.Default().Debug("logship: skipping symlink", "path", path)
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return fmt.Errorf("logship: rel %s: %w", path, err)
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	}); err != nil {
		return "", fmt.Errorf("logship: walk %s: %w", src, err)
	}

	return dest, nil
}

// sanitize keeps only [A-Za-z0-9._-] characters, replacing everything
// else with '_' so arbitrary runner names cannot escape the log tree. The
// bool reports whether any allowed character was present at all; a name
// with nothing but replacements is treated as invalid by the caller.
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

// copyFile copies src to dst with mode 0644, truncating any existing file.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("logship: open %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("logship: create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("logship: copy %s: %w", src, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("logship: close %s: %w", dst, err)
	}
	return nil
}
