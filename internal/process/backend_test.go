package process

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0xinterface/tentacles/internal/runner"
)

// fakeRunner is the stub agent, resolved from the package test directory.
const fakeRunner = "../../testdata/fake-runner/run.sh"

const jitValue = "fake-jit-value"

func writeEnvFile(t *testing.T, extra string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runner.env")
	content := "PATH=/usr/local/bin:/usr/bin:/bin\nHOME=/home/gha-runner\nLANG=C.UTF-8\n" + extra
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// newSlotDir creates a slot directory containing a run.sh symlink to the
// fake runner and a JIT file written via runner.WriteJIT.
func newSlotDir(t *testing.T) (dir, jitPath string) {
	t.Helper()
	dir = t.TempDir()
	// The target must be absolute: a relative symlink would resolve against
	// the temp slot dir, not this package's directory.
	abs, err := filepath.Abs(fakeRunner)
	if err != nil {
		t.Fatalf("Abs(%s): %v", fakeRunner, err)
	}
	if err := os.Symlink(abs, filepath.Join(dir, "run.sh")); err != nil {
		t.Fatalf("Symlink run.sh: %v", err)
	}
	jitPath = filepath.Join(dir, "runner.jit")
	if err := runner.WriteJIT(jitPath, jitValue); err != nil {
		t.Fatalf("WriteJIT: %v", err)
	}
	return dir, jitPath
}

func waitForMarker(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatalf("Stat marker: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("marker %s not created within 10s", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// checkMarker verifies the marker holds "<sha256(jit)> <pid>".
func checkMarker(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile marker: %v", err)
	}
	fields := strings.Fields(string(b))
	if len(fields) != 2 {
		t.Fatalf("marker = %q, want '<sha256> <pid>'", b)
	}
	sum := sha256.Sum256([]byte(jitValue))
	if want := hex.EncodeToString(sum[:]); fields[0] != want {
		t.Fatalf("marker digest = %q, want %q (sha256 of JIT value)", fields[0], want)
	}
	if _, err := strconv.Atoi(fields[1]); err != nil {
		t.Fatalf("marker pid %q not numeric: %v", fields[1], err)
	}
}

func TestBackendStartStop(t *testing.T) {
	slotDir, jitPath := newSlotDir(t)
	b := New(Options{EnvFile: writeEnvFile(t, "")})
	spec := runner.Spec{
		SlotDir:  slotDir,
		JITPath:  jitPath,
		UnitName: "tentacle-0001.service",
	}
	ctx := context.Background()

	if err := b.Start(ctx, spec); err != nil {
		t.Fatalf("Start: %v", err)
	}

	marker := filepath.Join(slotDir, ".fake-claimed")
	waitForMarker(t, marker)
	checkMarker(t, marker)

	active, err := b.Active(ctx)
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if len(active) != 1 || active[0] != spec.UnitName {
		t.Fatalf("Active = %v, want [%s]", active, spec.UnitName)
	}

	if err := b.Stop(ctx, spec.UnitName); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := b.Wait(ctx, spec.UnitName); err != nil {
		t.Fatalf("Wait after Stop: %v", err)
	}
	active, err = b.Active(ctx)
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("Active after Stop = %v, want []", active)
	}
}

func TestBackendWaitSelfExit(t *testing.T) {
	slotDir, jitPath := newSlotDir(t)
	// FAKE_RUNNER_SLEEP=0 makes the runner exit on its own right away.
	b := New(Options{EnvFile: writeEnvFile(t, "FAKE_RUNNER_SLEEP=0\n")})
	spec := runner.Spec{
		SlotDir:  slotDir,
		JITPath:  jitPath,
		UnitName: "tentacle-0001.service",
	}
	ctx := context.Background()

	if err := b.Start(ctx, spec); err != nil {
		t.Fatalf("Start: %v", err)
	}

	marker := filepath.Join(slotDir, ".fake-claimed")
	waitForMarker(t, marker)
	checkMarker(t, marker)

	if err := b.Wait(ctx, spec.UnitName); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	active, err := b.Active(ctx)
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("Active after exit = %v, want []", active)
	}
}

func TestUnknownUnit(t *testing.T) {
	b := New(Options{})
	ctx := context.Background()
	for _, unitName := range []string{"tentacle-9999.service", ""} {
		if err := b.Wait(ctx, unitName); err == nil {
			t.Fatalf("Wait(%q): expected error, got nil", unitName)
		} else if !strings.Contains(err.Error(), "unknown unit") {
			t.Fatalf("Wait(%q) error = %q, want 'unknown unit'", unitName, err)
		}
		if err := b.Stop(ctx, unitName); err == nil {
			t.Fatalf("Stop(%q): expected error, got nil", unitName)
		} else if !strings.Contains(err.Error(), "unknown unit") {
			t.Fatalf("Stop(%q) error = %q, want 'unknown unit'", unitName, err)
		}
	}
}

func TestStartMissingEnvFile(t *testing.T) {
	b := New(Options{EnvFile: filepath.Join(t.TempDir(), "nope.env")})
	slotDir, jitPath := newSlotDir(t)
	err := b.Start(context.Background(), runner.Spec{
		SlotDir:  slotDir,
		JITPath:  jitPath,
		UnitName: "tentacle-0001.service",
	})
	if err == nil {
		t.Fatal("Start: expected error for missing environment file, got nil")
	}
}

func TestStartEmptyUnitName(t *testing.T) {
	b := New(Options{})
	slotDir, jitPath := newSlotDir(t)
	err := b.Start(context.Background(), runner.Spec{SlotDir: slotDir, JITPath: jitPath})
	if err == nil {
		t.Fatal("Start: expected error for empty unit name, got nil")
	}
}

func TestStartDuplicateUnit(t *testing.T) {
	slotDir, jitPath := newSlotDir(t)
	b := New(Options{EnvFile: writeEnvFile(t, "")})
	spec := runner.Spec{SlotDir: slotDir, JITPath: jitPath, UnitName: "tentacle-0001.service"}
	ctx := context.Background()
	if err := b.Start(ctx, spec); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop(ctx, spec.UnitName)

	secondDir, secondJIT := newSlotDir(t)
	err := b.Start(ctx, runner.Spec{SlotDir: secondDir, JITPath: secondJIT, UnitName: spec.UnitName})
	if err == nil {
		t.Fatal("Start: expected error for duplicate unit name, got nil")
	}
}
