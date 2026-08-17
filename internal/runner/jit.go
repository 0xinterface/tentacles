package runner

import (
	"fmt"
	"os"
)

// WriteJIT writes encoded to path with mode 0600. The value is written to a
// temporary sibling and renamed over path so a concurrent reader never
// observes a partial file. The parent directory must already exist.
func WriteJIT(path, encoded string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(encoded), 0o600); err != nil {
		return fmt.Errorf("write JIT temp file: %w", err)
	}
	// WriteFile keeps the permissions of an existing file; force 0600 so a
	// stale temp can never widen the mode.
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("chmod JIT temp file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename JIT file into place: %w", err)
	}
	return nil
}
