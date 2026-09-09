// Package cleanup shreds the credential material a runner slot may
// leave behind (plan §10: "shred/unlink any leftover JIT or credential
// files"). The slot-directory teardown itself lives in internal/slot.
package cleanup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// credentialFiles are the files the official agent drops into a slot
// directory when it registers: the runner identity and the credentials
// used to talk to the service.
var credentialFiles = [...]string{".runner", ".credentials", ".credentials_rsaparams"}

// ShredCredentials truncates then removes the credential files in
// slotDir, so key material does not survive in unallocated blocks.
// A missing file is fine; other failures are joined into the returned
// error. Shredding is best-effort by design — the caller logs and
// proceeds with the wipe.
func ShredCredentials(slotDir string) error {
	var errs []error
	for _, name := range credentialFiles {
		if err := shredFile(filepath.Join(slotDir, name)); err != nil {
			errs = append(errs, err)
		}
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
