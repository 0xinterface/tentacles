package slot

import (
	"context"
	"errors"
	"testing"

	"github.com/0xinterface/tentacles/internal/runner"
)

type lateClaimErrorBackend struct {
	fakeBackend
	claim func()
}

func (b *lateClaimErrorBackend) Start(ctx context.Context, spec runner.Spec) error {
	if err := b.fakeBackend.Start(ctx, spec); err != nil {
		return err
	}
	return errors.New("launch response lost")
}

func (b *lateClaimErrorBackend) Stop(ctx context.Context, unit string) error {
	// Model a claim delivered while systemctl is connecting, before its stop
	// operation reaches the launched runner.
	b.claim()
	return b.fakeBackend.Stop(ctx, unit)
}

func TestClaimBeforeCompensatingStopPreserved(t *testing.T) {
	b := &lateClaimErrorBackend{}
	tab := NewTable(t.TempDir(), b, markerMaterialize, fakeJIT, t.TempDir(), nil)
	defer tab.Close()
	b.claim = func() {
		if !tab.Claim(ClaimJob{RunnerName: "debian-host-0001-ab12"}) {
			t.Fatal("claim not found")
		}
		slots := tab.Active()
		if len(slots) != 1 || slots[0].State != StateBusy {
			t.Fatalf("claim did not make runner busy: %+v", slots)
		}
	}
	if err := tab.Ensure(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	b.claim() // A late claim remains possible after the uncertain reply.
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.stopped) != 0 {
		t.Fatalf("claimed runner stopped after failed launch reply: %v", b.stopped)
	}
}
