package cleanup

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWipeRemovesEverything(t *testing.T) {
	dir := t.TempDir()
	jit := filepath.Join(dir, "0001.jit")
	if err := os.WriteFile(jit, []byte("encoded-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".runner", ".credentials", ".credentials_rsaparams"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("creds"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	work := filepath.Join(dir, "_work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "job-artifact"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Wipe(dir, jit); err != nil {
		t.Fatalf("Wipe: %v", err)
	}
	for _, gone := range []string{dir, jit} {
		if _, err := os.Stat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists (stat err: %v)", gone, err)
		}
	}
}

func TestWipeEmptySlotDirErrors(t *testing.T) {
	if err := Wipe("", ""); err == nil {
		t.Fatal("expected error for empty slot dir")
	}
}

func TestWipeNonexistentDirIsNil(t *testing.T) {
	if err := Wipe(filepath.Join(t.TempDir(), "nope"), ""); err != nil {
		t.Fatalf("Wipe nonexistent dir: %v", err)
	}
}

func TestWipeMissingJITIsNil(t *testing.T) {
	dir := t.TempDir()
	if err := Wipe(dir, filepath.Join(dir, "missing.jit")); err != nil {
		t.Fatalf("Wipe with missing jit: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("slot dir still exists: %v", err)
	}
}

func TestWipeCollectsErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-based failure does not apply to root")
	}
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "creds"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Read-only directory: truncate/remove inside it must fail on POSIX.
	if err := os.Chmod(blocked, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(blocked, 0o755) // let t.TempDir clean up

	if err := Wipe(dir, ""); err == nil {
		t.Fatal("expected collected errors")
	}
}
