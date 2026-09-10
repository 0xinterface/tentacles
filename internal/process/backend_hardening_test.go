package process

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xinterface/tentacles/internal/runner"
)

func TestStartHonorsCanceledContext(t *testing.T) {
	dir, jit := newSlotDir(t)
	b := New(Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	spec := runner.Spec{SlotDir: dir, JITPath: jit, UnitName: "canceled"}
	if err := b.Start(ctx, spec); !errors.Is(err, context.Canceled) {
		t.Errorf("Start=%v, want canceled", err)
		_ = b.Stop(context.Background(), spec.UnitName)
	}
}

func TestStartRejectsUnresolvedIdentity(t *testing.T) {
	dir, jit := newSlotDir(t)
	b := New(Options{})
	spec := runner.Spec{SlotDir: dir, JITPath: jit, UnitName: "unknown", User: "tentacles-user-that-does-not-exist"}
	if err := b.Start(context.Background(), spec); err == nil {
		t.Error("unresolvable user silently ignored")
		_ = b.Stop(context.Background(), spec.UnitName)
	}
}

func TestStartHonorsSpecEnvironment(t *testing.T) {
	dir, jit := newSlotDir(t)
	script := []byte("#!/bin/sh\nprintf '%s' \"$CHECK_VALUE\" > value\n")
	if err := os.Remove(filepath.Join(dir, "run.sh")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), script, 0700); err != nil {
		t.Fatal(err)
	}
	env := writeEnvFile(t, "CHECK_VALUE=from-spec\n")
	b := New(Options{})
	spec := runner.Spec{SlotDir: dir, JITPath: jit, UnitName: "environment", EnvFile: env}
	if err := b.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := b.Wait(ctx, spec.UnitName); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "value"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "from-spec" {
		t.Fatalf("environment=%q", got)
	}
}

func TestStopCancellationKillsProcessGroup(t *testing.T) {
	dir, jit := newSlotDir(t)
	if err := os.Remove(filepath.Join(dir, "run.sh")); err != nil {
		t.Fatal(err)
	}
	script := []byte("#!/bin/sh\ntrap '' TERM\n(sleep 1; touch survived) &\ntouch ready\nwhile :; do sleep 10; done\n")
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), script, 0700); err != nil {
		t.Fatal(err)
	}
	b := New(Options{}, WithStopTimeout(10*time.Second))
	spec := runner.Spec{SlotDir: dir, JITPath: jit, UnitName: "group"}
	if err := b.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	waitForMarker(t, filepath.Join(dir, "ready"))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := b.Stop(ctx, spec.UnitName)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop=%v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("Stop ignored caller deadline")
	}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	if err := b.Wait(waitCtx, spec.UnitName); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "survived")); !os.IsNotExist(err) {
		t.Fatal("descendant survived canceled stop")
	}
}

func TestDistinctUIDCanWriteSlotAndReadCredential(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to launch a genuinely distinct runner UID")
	}
	// /tmp is traversable by the test runner account; ordinary t.TempDir
	// ancestors deliberately are not. All test-owned names are removed below.
	base, err := os.MkdirTemp("/tmp", "tentacles-distinct-uid-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	if err := os.Chmod(base, 0755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "slot")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	jit := filepath.Join(base, "source.jit")
	if err := runner.WriteJIT(jit, "secret"); err != nil {
		t.Fatal(err)
	}
	script := []byte("#!/bin/sh\n[ \"$2\" = secret ] || exit 3\n[ \"$(id -u)\" = 65534 ] || exit 4\n[ ! -r ../source.jit ] || exit 5\nmkdir _work\nprintf 'ok' > _work/result\n")
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), script, 0755); err != nil {
		t.Fatal(err)
	}
	b := New(Options{})
	spec := runner.Spec{SlotDir: dir, JITPath: jit, User: "65534", Group: "65534", UnitName: "distinct"}
	if err := b.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.Wait(ctx, spec.UnitName); err != nil {
		t.Fatal(err)
	}
	result, err := os.ReadFile(filepath.Join(dir, "_work/result"))
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != "ok" {
		t.Fatalf("runner result=%q", result)
	}
	info, err := os.Stat(jit)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatal("source credential permissions widened")
	}
}
