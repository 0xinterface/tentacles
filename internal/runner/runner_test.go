package runner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestJITScript(t *testing.T) {
	got := JITScript()
	want := `jit=$(cat -- "$1") || exit; exec ./run.sh --jitconfig "$jit"`
	if got != want {
		t.Fatalf("JITScript() = %q, want %q", got, want)
	}
}

func TestBuildCommand(t *testing.T) {
	spec := Spec{SlotDir: "/srv/tentacles/pools/test/slots/0001", JITPath: "/run/tentacles/test/0001.jit"}
	cmd := BuildCommand(spec)
	if cmd.Dir != spec.SlotDir {
		t.Fatalf("Dir = %q, want %q", cmd.Dir, spec.SlotDir)
	}
	wantArgs := []string{"/bin/sh", "-c", JITScript(), "--", spec.JITPath}
	if len(cmd.Args) != len(wantArgs) {
		t.Fatalf("Args = %v, want %v", cmd.Args, wantArgs)
	}
	for i := range wantArgs {
		if cmd.Args[i] != wantArgs[i] {
			t.Fatalf("Args = %v, want %v", cmd.Args, wantArgs)
		}
	}
}

func TestWriteJIT(t *testing.T) {
	path := filepath.Join(t.TempDir(), "0001.jit")
	if err := WriteJIT(path, "secret-encoded"); err != nil {
		t.Fatalf("WriteJIT: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(b) != "secret-encoded" {
		t.Fatalf("content = %q, want %q", b, "secret-encoded")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %v, want 0600", got)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("%s.tmp should not remain, stat err = %v", path, err)
	}
}

func TestWriteJITOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "0001.jit")
	if err := WriteJIT(path, "first"); err != nil {
		t.Fatalf("WriteJIT(first): %v", err)
	}
	if err := WriteJIT(path, "second"); err != nil {
		t.Fatalf("WriteJIT(second): %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(b) != "second" {
		t.Fatalf("content = %q, want %q", b, "second")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode after overwrite = %v, want 0600", got)
	}
}
