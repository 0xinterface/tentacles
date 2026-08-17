package systemd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hkust/gh-runnerd/internal/runner"
)

// sampleSpec is the canonical slot configuration used by the golden
// Start-argument test.
func sampleSpec() runner.Spec {
	return runner.Spec{
		SlotDir:   "/var/lib/gh-runnerd/slots/0001",
		WorkDir:   "/var/lib/gh-runnerd/slots/0001/_work",
		JITPath:   "/run/gh-runnerd/0001.jit",
		EnvFile:   "/etc/gh-runnerd/runner.env",
		User:      "gha-runner",
		Group:     "gha-runner",
		CPUQuota:  "400%",
		MemoryMax: "8G",
		UnitName:  "gha-slot-0001.service",
	}
}

// writeFakeBin writes an executable sh script into dir and returns its
// path. Fake binaries must work on darwin, so they are plain /bin/sh.
func writeFakeBin(t *testing.T, dir, name, script string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

const fakeSystemdRun = `
for arg in "$@"; do
	printf '%s\n' "$arg" >> "$FAKE_SYSTEMD_RUN_LOG"
done
if [ -n "$FAKE_SYSTEMD_RUN_STDERR" ]; then
	printf '%s\n' "$FAKE_SYSTEMD_RUN_STDERR" >&2
fi
exit "${FAKE_SYSTEMD_RUN_EXIT:-0}"
`

const fakeSystemctl = `
for arg in "$@"; do
	printf '%s\n' "$arg" >> "$FAKE_SYSTEMCTL_LOG"
done
case "$1" in
	wait)
		if [ "${FAKE_SYSTEMCTL_WAIT_FAIL:-0}" = "1" ]; then
			printf 'Unknown operation wait.\n' >&2
			exit 1
		fi
		exit 0
		;;
	is-active)
		if [ -n "$FAKE_SYSTEMCTL_COUNT_FILE" ]; then
			n=0
			[ -f "$FAKE_SYSTEMCTL_COUNT_FILE" ] && n=$(cat "$FAKE_SYSTEMCTL_COUNT_FILE")
			n=$((n + 1))
			printf '%s\n' "$n" > "$FAKE_SYSTEMCTL_COUNT_FILE"
			if [ "$n" -lt "${FAKE_SYSTEMCTL_FLIP_AT:-2}" ]; then
				printf 'active\n'
				exit 0
			fi
		fi
		printf '%s\n' "${FAKE_SYSTEMCTL_STATE:-inactive}"
		exit 0
		;;
	list-units)
		if [ "${FAKE_SYSTEMCTL_LIST_FAIL:-0}" = "1" ]; then
			[ -n "$FAKE_SYSTEMCTL_STDERR" ] && printf '%s\n' "$FAKE_SYSTEMCTL_STDERR" >&2
			exit 1
		fi
		printf '%s\n' "$FAKE_SYSTEMCTL_LIST"
		exit 0
		;;
esac
if [ -n "$FAKE_SYSTEMCTL_STDERR" ]; then
	printf '%s\n' "$FAKE_SYSTEMCTL_STDERR" >&2
fi
exit "${FAKE_SYSTEMCTL_EXIT:-0}"
`

// newTestBackend wires a Backend to fake binaries that log their argv.
func newTestBackend(t *testing.T) (*Backend, string, string) {
	t.Helper()
	binDir := t.TempDir()
	runLog := filepath.Join(t.TempDir(), "systemd-run.log")
	ctlLog := filepath.Join(t.TempDir(), "systemctl.log")
	t.Setenv("FAKE_SYSTEMD_RUN_LOG", runLog)
	t.Setenv("FAKE_SYSTEMCTL_LOG", ctlLog)
	runBin := writeFakeBin(t, binDir, "systemd-run", fakeSystemdRun)
	ctlBin := writeFakeBin(t, binDir, "systemctl", fakeSystemctl)
	b := New(Options{
		SystemdRunBin: runBin,
		SystemctlBin:  ctlBin,
		StopTimeout:   30 * time.Second,
	})
	return b, runLog, ctlLog
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestStartGoldenArgVector(t *testing.T) {
	b, runLog, _ := newTestBackend(t)
	spec := sampleSpec()

	if err := b.Start(context.Background(), spec); err != nil {
		t.Fatalf("Start: %v", err)
	}

	want := []string{
		"--collect",
		"--unit", "gha-slot-0001.service",
		"--description", "GitHub Actions runner slot",
		"-p", "Type=exec",
		"-p", "User=gha-runner",
		"-p", "Group=gha-runner",
		"-p", "WorkingDirectory=/var/lib/gh-runnerd/slots/0001",
		"-p", "EnvironmentFile=/etc/gh-runnerd/runner.env",
		"-p", "CPUQuota=400%",
		"-p", "MemoryMax=8G",
		"-p", "Nice=5",
		"-p", "KillMode=mixed",
		"-p", "TimeoutStopSec=30",
		"-p", "TasksMax=4096",
		"-p", "PrivateTmp=yes",
		"-p", "NoNewPrivileges=yes",
		"-p", "ProtectSystem=strict",
		"-p", "ProtectHome=read-only",
		"-p", "ReadWritePaths=/var/lib/gh-runnerd/slots/0001:/tmp",
		"-p", "RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6",
		"-p", "LockPersonality=yes",
		"/bin/sh", "-c", runner.JITScript(spec.JITPath), "--", spec.JITPath,
	}

	if got := readLog(t, runLog); got != strings.Join(want, "\n")+"\n" {
		t.Fatalf("Start argv mismatch\n--- got ---\n%s\n--- want ---\n%s", got, strings.Join(want, "\n"))
	}
}

func TestStartSkipsEmptyProperties(t *testing.T) {
	b, runLog, _ := newTestBackend(t)
	spec := runner.Spec{
		SlotDir:  "/var/lib/gh-runnerd/slots/0002",
		JITPath:  "/run/gh-runnerd/0002.jit",
		UnitName: "gha-slot-0002.service",
		// User, Group, EnvFile, CPUQuota, MemoryMax intentionally empty
	}

	if err := b.Start(context.Background(), spec); err != nil {
		t.Fatalf("Start: %v", err)
	}

	log := readLog(t, runLog)
	for _, absent := range []string{"User=", "Group=", "EnvironmentFile=", "CPUQuota=", "MemoryMax="} {
		if strings.Contains(log, absent) {
			t.Errorf("log contains %q, expected skipped", absent)
		}
	}
	for _, present := range []string{"WorkingDirectory=/var/lib/gh-runnerd/slots/0002", "TimeoutStopSec=30", "Type=exec"} {
		if !strings.Contains(log, present) {
			t.Errorf("log missing %q", present)
		}
	}
}

func TestStartErrorIncludesStderrTail(t *testing.T) {
	b, _, _ := newTestBackend(t)
	t.Setenv("FAKE_SYSTEMD_RUN_EXIT", "1")
	t.Setenv("FAKE_SYSTEMD_RUN_STDERR", "Failed to start transient service unit: Operation refused")

	err := b.Start(context.Background(), sampleSpec())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "gha-slot-0001.service") {
		t.Errorf("error missing unit name: %v", err)
	}
	if !strings.Contains(err.Error(), "Operation refused") {
		t.Errorf("error missing stderr tail: %v", err)
	}
}

func TestStopArgvAndError(t *testing.T) {
	b, _, ctlLog := newTestBackend(t)

	if err := b.Stop(context.Background(), "gha-slot-0001.service"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if want := "stop\ngha-slot-0001.service\n"; readLog(t, ctlLog) != want {
		t.Fatalf("Stop argv = %q, want %q", readLog(t, ctlLog), want)
	}

	t.Setenv("FAKE_SYSTEMCTL_EXIT", "1")
	t.Setenv("FAKE_SYSTEMCTL_STDERR", "Failed to stop unit: Connection timed out")
	err := b.Stop(context.Background(), "gha-slot-0001.service")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "Connection timed out") {
		t.Errorf("error missing stderr: %v", err)
	}
}

func TestWaitHappyPath(t *testing.T) {
	b, _, ctlLog := newTestBackend(t)

	if err := b.Wait(context.Background(), "gha-slot-0001.service"); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if want := "wait\ngha-slot-0001.service\n"; readLog(t, ctlLog) != want {
		t.Fatalf("Wait argv = %q, want %q", readLog(t, ctlLog), want)
	}
}

func TestWaitFallbackOnUnknownOperation(t *testing.T) {
	b, _, ctlLog := newTestBackend(t)
	t.Setenv("FAKE_SYSTEMCTL_WAIT_FAIL", "1")
	t.Setenv("FAKE_SYSTEMCTL_STATE", "inactive")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.Wait(ctx, "gha-slot-0001.service"); err != nil {
		t.Fatalf("Wait fallback: %v", err)
	}

	log := readLog(t, ctlLog)
	want := "wait\ngha-slot-0001.service\nis-active\ngha-slot-0001.service\n"
	if log != want {
		t.Fatalf("Wait fallback argv = %q, want %q", log, want)
	}
}

func TestWaitFallbackPollingUntilGone(t *testing.T) {
	b, _, ctlLog := newTestBackend(t)
	t.Setenv("FAKE_SYSTEMCTL_WAIT_FAIL", "1")
	// First is-active poll reports active, the second flips to inactive:
	// deterministic regardless of poll timing.
	t.Setenv("FAKE_SYSTEMCTL_COUNT_FILE", filepath.Join(t.TempDir(), "count"))
	t.Setenv("FAKE_SYSTEMCTL_FLIP_AT", "2")
	t.Setenv("FAKE_SYSTEMCTL_STATE", "inactive")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.Wait(ctx, "gha-slot-0001.service"); err != nil {
		t.Fatalf("Wait fallback: %v", err)
	}
	log := readLog(t, ctlLog)
	if got, want := strings.Count(log, "is-active"), 2; got != want {
		t.Fatalf("expected %d is-active polls, got %d:\n%s", want, got, log)
	}
}

func TestWaitFallbackContextCancellation(t *testing.T) {
	b, _, _ := newTestBackend(t)
	t.Setenv("FAKE_SYSTEMCTL_WAIT_FAIL", "1")
	t.Setenv("FAKE_SYSTEMCTL_STATE", "active") // never goes inactive

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	err := b.Wait(ctx, "gha-slot-0001.service")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait fallback err = %v, want context.DeadlineExceeded", err)
	}
}

func TestActiveParsesLegendOutput(t *testing.T) {
	b, _, _ := newTestBackend(t)
	t.Setenv("FAKE_SYSTEMCTL_LIST",
		"gha-slot-0001.service loaded active running GitHub Actions runner slot\n"+
			"gha-slot-0002.service loaded active running GitHub Actions runner slot\n"+
			"not-a-slot loaded active running something else\n")

	got, err := b.Active(context.Background())
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	want := []string{"gha-slot-0001.service", "gha-slot-0002.service"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Active = %v, want %v", got, want)
	}
}

func TestActiveEmptyOutput(t *testing.T) {
	b, _, _ := newTestBackend(t)
	t.Setenv("FAKE_SYSTEMCTL_LIST", "")

	got, err := b.Active(context.Background())
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Active = %v, want empty", got)
	}
}

func TestActiveReturnsCommandError(t *testing.T) {
	b, _, _ := newTestBackend(t)
	t.Setenv("FAKE_SYSTEMCTL_LIST_FAIL", "1")
	t.Setenv("FAKE_SYSTEMCTL_STDERR", "Failed to create bus connection: No such file or directory")

	_, err := b.Active(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "No such file") {
		t.Errorf("error missing stderr: %v", err)
	}
}
