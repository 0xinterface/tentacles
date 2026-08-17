package logship

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestShipDiagCopiesNestedFiles(t *testing.T) {
	slotDir := t.TempDir()
	writeTree(t, slotDir, map[string]string{
		"_diag/worker.log":         "alpha",
		"_diag/nested/task.log":    "beta",
		"_diag/nested/deep/result": "gamma",
		"other-ignored":            "delta", // at slot root, outside _diag: not shipped
	})
	logDir := t.TempDir()

	dest, err := ShipDiag(slotDir, logDir, "debian-host-0001-abc")
	if err != nil {
		t.Fatalf("ShipDiag: %v", err)
	}
	if dest == "" {
		t.Fatal("expected non-empty dest")
	}

	base := filepath.Base(dest)
	slotID := filepath.Base(slotDir)
	pat := regexp.MustCompile(`^` + regexp.QuoteMeta(slotID) + `-\d{8}-\d{6}-debian-host-0001-abc$`)
	if !pat.MatchString(base) {
		t.Fatalf("dest name %q does not match %v", base, pat)
	}
	if filepath.Dir(dest) != logDir {
		t.Fatalf("dest dir = %q, want %q", filepath.Dir(dest), logDir)
	}

	for name, want := range map[string]string{
		"worker.log":         "alpha",
		"nested/task.log":    "beta",
		"nested/deep/result": "gamma",
	} {
		got, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	// Files land 0644 regardless of source mode.
	info, err := os.Stat(filepath.Join(dest, "worker.log"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("copied mode = %o, want 644", perm)
	}
	dirInfo, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o755 {
		t.Errorf("dest dir mode = %o, want 755", perm)
	}

	if _, err := os.Stat(filepath.Join(logDir, "other-ignored")); !os.IsNotExist(err) {
		t.Error("file outside _diag was copied")
	}
}

func TestShipDiagMissingDiagIsNil(t *testing.T) {
	slotDir := t.TempDir()
	dest, err := ShipDiag(slotDir, t.TempDir(), "runner")
	if err != nil {
		t.Fatalf("ShipDiag: %v", err)
	}
	if dest != "" {
		t.Fatalf("dest = %q, want empty", dest)
	}
}

func TestShipDiagSanitizesRunnerName(t *testing.T) {
	slotDir := t.TempDir()
	writeTree(t, slotDir, map[string]string{"_diag/a.log": "x"})
	logDir := t.TempDir()

	dest, err := ShipDiag(slotDir, logDir, "weird name/with!bad@chars")
	if err != nil {
		t.Fatalf("ShipDiag: %v", err)
	}
	if !strings.HasSuffix(filepath.Base(dest), "-weird_name_with_bad_chars") {
		t.Fatalf("dest base %q not sanitized as expected", filepath.Base(dest))
	}
}

func TestShipDiagEmptyRunnerNameBecomesUnknown(t *testing.T) {
	slotDir := t.TempDir()
	writeTree(t, slotDir, map[string]string{"_diag/a.log": "x"})
	logDir := t.TempDir()

	dest, err := ShipDiag(slotDir, logDir, "")
	if err != nil {
		t.Fatalf("ShipDiag: %v", err)
	}
	if !strings.HasSuffix(filepath.Base(dest), "-unknown") {
		t.Fatalf("dest base %q, want suffix -unknown", filepath.Base(dest))
	}

	dest2, err := ShipDiag(slotDir, logDir, "!!!")
	if err != nil {
		t.Fatalf("ShipDiag: %v", err)
	}
	if !strings.HasSuffix(filepath.Base(dest2), "-unknown") {
		t.Fatalf("dest base %q, want suffix -unknown for fully invalid name", filepath.Base(dest2))
	}
}

func TestShipDiagSkipsSymlinks(t *testing.T) {
	slotDir := t.TempDir()
	writeTree(t, slotDir, map[string]string{
		"_diag/real.log": "content",
	})
	// Symlink inside _diag pointing outside the tree.
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(slotDir, "_diag", "link.log")); err != nil {
		t.Fatal(err)
	}
	// Symlink to a directory inside _diag: must not be followed/descended.
	if err := os.Symlink(filepath.Join(slotDir, "_diag"), filepath.Join(slotDir, "_diag", "linkdir")); err != nil {
		t.Fatal(err)
	}

	logDir := t.TempDir()
	dest, err := ShipDiag(slotDir, logDir, "runner")
	if err != nil {
		t.Fatalf("ShipDiag: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dest, "link.log")); !os.IsNotExist(err) {
		t.Error("symlinked file was copied (followed)")
	}
	if _, err := os.Stat(filepath.Join(dest, "linkdir")); !os.IsNotExist(err) {
		t.Error("symlinked directory was copied (followed)")
	}
	content, err := os.ReadFile(filepath.Join(dest, "real.log"))
	if err != nil || string(content) != "content" {
		t.Errorf("real.log not copied intact: %v %q", err, content)
	}
}

func TestShipDiagEmptyDirsError(t *testing.T) {
	if _, err := ShipDiag("", "", "runner"); err == nil {
		t.Fatal("expected error for empty dirs")
	}
}
