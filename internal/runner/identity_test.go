package runner

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestPrepareSlotRejectsHardlinksWithoutMutatingTarget(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("sentinel"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}
	identity, err := ResolveIdentity(Spec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := PrepareSlot(context.Background(), Spec{SlotDir: dir}, identity); err == nil {
		t.Fatal("accepted hardlinked slot file")
	}
	info, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0640 || int(info.Sys().(*syscall.Stat_t).Uid) != os.Geteuid() {
		t.Fatal("outside metadata changed")
	}
}

func TestPrepareSlotDoesNotFollowSymlinks(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "sentinel")
	if err := os.WriteFile(sentinel, []byte("sentinel"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}
	identity, err := ResolveIdentity(Spec{})
	if err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		identity.UID = 65534
		identity.GID = 65534
	}
	if err := PrepareSlot(context.Background(), Spec{SlotDir: dir}, identity); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0640 || int(info.Sys().(*syscall.Stat_t).Uid) != os.Geteuid() {
		t.Fatal("symlink target metadata changed")
	}
}

func TestPrepareSlotCanceledLeavesOwnershipUntouched(t *testing.T) {
	dir := t.TempDir()
	before, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := PrepareSlot(ctx, Spec{SlotDir: dir}, Identity{UID: 65534, GID: 65534}); err != context.Canceled {
		t.Fatalf("PrepareSlot=%v", err)
	}
	after, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode() != after.Mode() || before.Sys().(*syscall.Stat_t).Uid != after.Sys().(*syscall.Stat_t).Uid {
		t.Fatal("canceled preparation mutated slot")
	}
}

func TestReadJITRejectsUnsafeSource(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	if err := WriteJIT(source, "secret"); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadJIT(link); err == nil {
		t.Fatal("accepted symlink source")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(source, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadJIT(source); err == nil {
		t.Fatal("accepted hardlink source")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(source, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadJIT(source); err == nil {
		t.Fatal("accepted world-readable source")
	}
}
