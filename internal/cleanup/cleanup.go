// Package cleanup removes credential names left by a runner slot.
// Unlinking does not guarantee secure erasure of the underlying storage.
package cleanup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

var credentialFiles = [...]string{".runner", ".credentials", ".credentials_rsaparams"}

// ShredCredentials is retained for compatibility. It unlinks credential names
// without opening or truncating their targets. Missing slots/files are harmless;
// other errors are joined so every credential name gets a removal attempt.
func ShredCredentials(slotDir string) error {
	fd, err := unix.Open(filepath.Clean(slotDir), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cleanup: open slot: %w", err)
	}
	defer unix.Close(fd)

	errs := []error{}
	for _, name := range credentialFiles {
		if err := unix.Unlinkat(fd, name, 0); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("cleanup: unlink %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}
