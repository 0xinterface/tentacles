package runner

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteJIT writes encoded to path with mode 0600. The value is written to a
// temporary sibling and renamed over path so a concurrent reader never
// observes a partial file. The parent directory must already exist.
func WriteJIT(path, encoded string) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".jit-*")
	if err != nil {
		return fmt.Errorf("create JIT temp file: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.WriteString(encoded); err != nil {
		_ = f.Close()
		return fmt.Errorf("write JIT temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close JIT temp file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename JIT file into place: %w", err)
	}
	return nil
}
