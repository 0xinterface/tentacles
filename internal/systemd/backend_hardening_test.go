package systemd

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestWritablePathsAndCredentialArguments(t *testing.T) {
	b := New(Options{})
	spec := sampleSpec()
	spec.User = ""
	spec.Group = ""
	spec.SlotDir = `/srv/slots/a b"c%h`
	spec.JITPath = `/run/a b%h.jit`
	args := b.startArgs(spec)
	joined := strings.Join(args, "\n")
	for _, want := range []string{`ReadWritePaths="/srv/slots/a b\"c%h" "/tmp"`, `LoadCredential=jit:/run/a b%h.jit`, `$(cat "$CREDENTIALS_DIRECTORY/jit")`} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
}

func TestWaitRejectsUntrustworthyObservation(t *testing.T) {
	for _, script := range []string{"printf 'inactive\\n'; exit 1", "printf 'gibberish\\n'", "printf 'inactive\\n' >&2; exit 1", "printf 'LoadState=loaded\\nActiveState=inactive\\n'; exit 1", "printf 'LoadState=\\nLoadState=loaded\\nActiveState=inactive\\n'"} {
		t.Run(script, func(t *testing.T) {
			bin := writeFakeBin(t, t.TempDir(), "systemctl", script)
			b := New(Options{SystemctlBin: bin})
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if err := b.Wait(ctx, "tentacle-default-0001.service"); err == nil {
				t.Fatal("uncertain observation confirmed exit")
			}
		})
	}
}

func TestActiveRejectsMalformedOutput(t *testing.T) {
	b, _, _ := newTestBackend(t)
	for _, line := range []string{"garbage", "tentacle-default-0001.service", "tentacle-0001.service loaded nonsense running"} {
		t.Setenv("FAKE_SYSTEMCTL_LIST", line)
		if _, err := b.Active(context.Background()); err == nil {
			t.Errorf("accepted malformed output %q", line)
		}
	}
}

func TestWaitConfirmsCollectedUnit(t *testing.T) {
	bin := writeFakeBin(t, t.TempDir(), "systemctl", "printf 'LoadState=not-found\\nActiveState=inactive\\n'")
	b := New(Options{SystemctlBin: bin})
	if err := b.Wait(context.Background(), "tentacle-default-0001.service"); err != nil {
		t.Fatal(err)
	}
}

func TestWaitCancelsSystemctlDescendants(t *testing.T) {
	bin := writeFakeBin(t, t.TempDir(), "systemctl", "sleep 10 & wait")
	b := New(Options{SystemctlBin: bin})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := b.Wait(ctx, "tentacle-default-0001.service"); err != context.DeadlineExceeded {
		t.Fatalf("Wait=%v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("inherited pipe held observation past cancellation")
	}
}

func TestStartMissingSpecEnvironmentFailsClosed(t *testing.T) {
	b, runLog, _ := newTestBackend(t)
	spec := testStartSpec(t)
	spec.EnvFile += ".missing"
	if err := b.Start(context.Background(), spec); err == nil {
		t.Fatal("missing environment accepted")
	}
	if _, err := os.Stat(runLog); !os.IsNotExist(err) {
		t.Fatal("launched unit despite unavailable environment")
	}
}
