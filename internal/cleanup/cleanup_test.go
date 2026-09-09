package cleanup

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestShredRemovesCredentialFilesOnly(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{".runner", ".credentials", ".credentials_rsaparams"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("creds"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	keep := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(keep, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := ShredCredentials(dir); err != nil {
		t.Fatalf("ShredCredentials: %v", err)
	}
	for _, name := range []string{".runner", ".credentials", ".credentials_rsaparams"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists (stat err: %v)", name, err)
		}
	}
	// Shredding is surgical: the slot tree is removed by the caller.
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("run.sh removed by shred: %v", err)
	}
}

func TestShredMissingFilesIsNil(t *testing.T) {
	if err := ShredCredentials(filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Fatalf("ShredCredentials on missing dir: %v", err)
	}
}

func TestShredCollectsErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-based failure does not apply to root")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".runner"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Read-only directory: truncate/remove inside it must fail on POSIX.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755) // let t.TempDir clean up

	if err := ShredCredentials(dir); err == nil {
		t.Fatal("expected collected errors")
	}
}
