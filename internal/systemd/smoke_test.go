//go:build linux

package systemd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/0xinterface/tentacles/internal/runner"
)

// Opt in only on a disposable systemd host. This exercises real transient
// units and a distinct UID; it never mints a GitHub credential.
func TestRealSystemdSmoke(t *testing.T) {
	if os.Getenv("TENTACLES_SYSTEMD_SMOKE") != "1" {
		t.Skip("set TENTACLES_SYSTEMD_SMOKE=1 on a disposable systemd host")
	}
	if os.Geteuid() != 0 {
		t.Fatal("smoke requires root")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := CheckSupport(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		t.Fatal("a running system systemd manager is required")
	}
	who, err := runner.ResolveIdentity(runner.Spec{User: "nobody"})
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/var/lib", "tentacles-smoke-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	unit := "tentacle-default-" + strconv.FormatInt(time.Now().UnixNano(), 10) + ".service"
	t.Logf("isolated unit %s", unit)
	backend := New(Options{StopTimeout: 5 * time.Second})
	attempted := false
	defer func() {
		if attempted {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer stopCancel()
			if err := backend.Stop(stopCtx, unit); err != nil {
				if err := backend.Wait(stopCtx, unit); err != nil {
					t.Errorf("could not confirm smoke unit exit; retained %s: %v", root, err)
					return
				}
			}
		}
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	}()
	home := filepath.Join(root, "home")
	template := filepath.Join(root, "template")
	slotDir := filepath.Join(root, "slot space %literal")
	jitDir := filepath.Join(root, "jit")
	for _, dir := range []string{home, template, slotDir, jitDir} {
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chown(home, who.UID, who.GID); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(jitDir, 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(jitDir, "source")
	if err := runner.WriteJIT(source, "smoke-credential"); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(root, "runner.env")
	envBody := fmt.Sprintf("PATH=/usr/bin:/bin\nHOME=%s\nEXPECTED_UID=%d\nSOURCE=%s\nTEMPLATE=%s\n", home, who.UID, source, template)
	if err := os.WriteFile(envFile, []byte(envBody), 0600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
set -eu
[ "$1" = --jitconfig ]
[ "$2" = smoke-credential ]
[ "$(id -u)" = "$EXPECTED_UID" ]
[ ! -r "$SOURCE" ]
if touch "$TEMPLATE/forbidden" 2>/dev/null; then exit 20; fi
touch "$HOME/.cache/allowed"
printf 'ready\n' > smoke-ready
trap 'exit 0' TERM
while :; do sleep 1; done
`
	if err := os.WriteFile(filepath.Join(slotDir, "run.sh"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	attempted = true
	if err := backend.Start(ctx, runner.Spec{UnitName: unit, SlotDir: slotDir, JITPath: source, User: "nobody", EnvFile: envFile, CPUQuota: "100%", MemoryMax: "128M"}); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(slotDir, "smoke-ready")); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("runner did not pass sandbox/credential checks; inspect journalctl -u %s", unit)
		case <-time.After(25 * time.Millisecond):
		}
	}
	// A fresh backend can rediscover the running unit after supervisor restart.
	restarted := New(Options{StopTimeout: 5 * time.Second})
	units, err := restarted.Active(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(units, unit) {
		t.Fatal("fresh backend did not discover running unit")
	}
	if err := restarted.Stop(ctx, unit); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Wait(ctx, unit); err != nil {
		t.Fatal(err)
	}
	t.Log("distinct UID, private credentials, slot/cache writes, template protection, rediscovery and confirmed stop passed")
}
