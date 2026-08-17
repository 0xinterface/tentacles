// Package cleanup removes every trace of a runner slot after its process
// exits: the JIT config, any credential material the agent may have
// dropped, and the slot directory itself.
package cleanup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Wipe deletes a slot: first the JIT config file (if jitPath is set),
// then any leftover credential files in the slot tree (truncated before
// removal so the key material is not left in slack space), then the whole
// slot directory. Shredding is best-effort — failures are collected, not
// fatal — but removal of the slot directory itself must succeed. All
// collected failures are joined into the returned error.
func Wipe(slotDir, jitPath string) error {
	if slotDir == "" {
		return errors.New("cleanup: empty slot directory")
	}
	var errs []error

	if jitPath != "" {
		if err := os.Remove(jitPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("cleanup: remove jit %s: %w", jitPath, err))
		}
	}

	for _, name := range []string{".runner", ".credentials", ".credentials_rsaparams"} {
		if err := shredFile(filepath.Join(slotDir, name)); err != nil {
			errs = append(errs, err)
		}
	}

	if err := os.RemoveAll(slotDir); err != nil {
		errs = append(errs, fmt.Errorf("cleanup: remove slot dir %s: %w", slotDir, err))
	}
	return errors.Join(errs...)
}

// shredFile truncates a file to zero length and removes it, so sensitive
// contents do not survive in unallocated blocks. A missing file is fine.
func shredFile(path string) error {
	if err := os.Truncate(path, 0); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("cleanup: truncate %s: %w", path, err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cleanup: remove %s: %w", path, err)
	}
	return nil
}
