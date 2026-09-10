package runner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCommandTreatsJITPathAsData(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s' \"$2\" > received\n"
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config'$(touch injected)' with spaces.jit")
	if err := WriteJIT(path, "exact-secret"); err != nil {
		t.Fatal(err)
	}
	if out, err := BuildCommand(Spec{SlotDir: dir, JITPath: path}).CombinedOutput(); err != nil {
		t.Fatalf("command: %v: %s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(dir, "received"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "exact-secret" {
		t.Fatalf("credential = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "injected")); !os.IsNotExist(err) {
		t.Fatal("JIT path executed shell syntax")
	}
}

func TestWriteJITDoesNotFollowStaleTemporaryLink(t *testing.T) {
	for _, hard := range []bool{false, true} {
		t.Run(map[bool]string{false: "symlink", true: "hardlink"}[hard], func(t *testing.T) {
			dir := t.TempDir()
			outside := filepath.Join(dir, "sentinel")
			path := filepath.Join(dir, "runner.jit")
			if err := os.WriteFile(outside, []byte("untouched"), 0644); err != nil {
				t.Fatal(err)
			}
			link := os.Symlink
			if hard {
				link = os.Link
			}
			if err := link(outside, path+".tmp"); err != nil {
				t.Fatal(err)
			}
			if err := WriteJIT(path, "secret"); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(outside)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "untouched" {
				t.Fatalf("outside file changed to %q", got)
			}
			info, err := os.Stat(outside)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0644 {
				t.Fatal("outside mode changed")
			}
		})
	}
}
