//go:build linux

package systemd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0xinterface/tentacles/internal/runner"
)

const (
	smokeWorkerEnv      = "TENTACLES_SYSTEMD_SMOKE_WORKER"
	smokeRootEnv        = "TENTACLES_SYSTEMD_SMOKE_ROOT"
	smokeSlotUnitEnv    = "TENTACLES_SYSTEMD_SMOKE_SLOT_UNIT"
	smokeServiceUnitEnv = "TENTACLES_SYSTEMD_SERVICE_UNIT"
)

var supervisorSmokePropertyNames = map[string]struct{}{
	"Group":                   {},
	"LockPersonality":         {},
	"NoNewPrivileges":         {},
	"PrivateTmp":              {},
	"ProtectControlGroups":    {},
	"ProtectHome":             {},
	"ProtectKernelLogs":       {},
	"ProtectKernelModules":    {},
	"ProtectKernelTunables":   {},
	"ProtectSystem":           {},
	"RestrictAddressFamilies": {},
	"RestrictSUIDSGID":        {},
	"UMask":                   {},
	"User":                    {},
}

var requiredSupervisorSmokeProperties = []string{
	"LockPersonality",
	"NoNewPrivileges",
	"PrivateTmp",
	"ProtectControlGroups",
	"ProtectHome",
	"ProtectKernelLogs",
	"ProtectKernelModules",
	"ProtectKernelTunables",
	"ProtectSystem",
	"RestrictAddressFamilies",
	"RestrictSUIDSGID",
	"UMask",
}

// Opt in only on a disposable systemd host. The outer test starts this same
// test binary in a transient unit carrying the shipped supervisor hardening.
// The worker then exercises a real slot unit and a distinct UID without
// minting a GitHub credential.
func TestRealSystemdSmoke(t *testing.T) {
	if os.Getenv("TENTACLES_SYSTEMD_SMOKE") != "1" {
		t.Skip("set TENTACLES_SYSTEMD_SMOKE=1 on a disposable systemd host")
	}
	if os.Geteuid() != 0 {
		t.Fatal("smoke requires root")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := CheckSupport(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		t.Fatal("a running system systemd manager is required")
	}
	if os.Getenv(smokeWorkerEnv) == "1" {
		runRealSystemdSmoke(
			t,
			ctx,
			os.Getenv(smokeRootEnv),
			os.Getenv(smokeSlotUnitEnv),
		)
		return
	}

	serviceUnit := os.Getenv(smokeServiceUnitEnv)
	if serviceUnit == "" {
		serviceUnit = "configs/systemd/tentacles.service"
	}
	properties, err := supervisorSmokeProperties(serviceUnit)
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
	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	slotUnit := "tentacle-default-" + suffix + ".service"
	supervisorUnit := "tentacles-smoke-supervisor-" + suffix + ".service"
	t.Logf("isolated supervisor %s and slot %s", supervisorUnit, slotUnit)
	defer cleanupSmoke(
		t,
		root,
		supervisorUnit,
		slotUnit,
	)

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	worker := filepath.Join(root, "systemd.test")
	if err := copyExecutable(executable, worker); err != nil {
		t.Fatal(err)
	}

	args := []string{
		"--quiet",
		"--wait",
		"--pipe",
		"--collect",
		"--unit", supervisorUnit,
		"-p", "Type=exec",
		"-p", "ReadWritePaths=" + root,
		"--setenv=TENTACLES_SYSTEMD_SMOKE=1",
		"--setenv=" + smokeWorkerEnv + "=1",
		"--setenv=" + smokeRootEnv + "=" + root,
		"--setenv=" + smokeSlotUnitEnv + "=" + slotUnit,
	}
	for _, property := range properties {
		args = append(args, "-p", property)
	}
	args = append(args, worker, "-test.run=^TestRealSystemdSmoke$", "-test.v")
	cmd := runner.CommandContext(ctx, "systemd-run", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("supervisor smoke under %s: %v\n%s", serviceUnit, err, out)
	}
	t.Logf("supervisor smoke output:\n%s", out)
}

func runRealSystemdSmoke(t *testing.T, ctx context.Context, root, unit string) {
	t.Helper()
	if root == "" || unit == "" {
		t.Fatal("smoke worker environment is incomplete")
	}
	who, err := runner.ResolveIdentity(runner.Spec{User: "nobody"})
	if err != nil {
		t.Fatal(err)
	}
	backend := New(Options{StopTimeout: 5 * time.Second})
	attempted := false
	defer func() {
		if !attempted {
			return
		}
		if err := stopSmokeSlot(backend, unit); err != nil {
			t.Errorf("could not confirm smoke unit exit; retained %s: %v", root, err)
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
	envBody := fmt.Sprintf(
		"PATH=/usr/bin:/bin\nHOME=%s\nEXPECTED_UID=%d\nSOURCE=%s\nTEMPLATE=%s\n",
		home,
		who.UID,
		source,
		template,
	)
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
	if err := backend.Start(
		ctx,
		runner.Spec{
			UnitName:  unit,
			SlotDir:   slotDir,
			JITPath:   source,
			User:      "nobody",
			EnvFile:   envFile,
			CPUQuota:  "100%",
			MemoryMax: "128M",
		},
	); err != nil {
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

func supervisorSmokeProperties(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open service unit %q: %w", path, err)
	}
	defer file.Close()

	properties := []string{}
	seen := map[string]struct{}{}
	inService := false
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inService = line == "[Service]"
			continue
		}
		if !inService || line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, _, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("malformed service directive %q", line)
		}
		if _, ok := supervisorSmokePropertyNames[name]; !ok {
			continue
		}
		properties = append(properties, line)
		seen[name] = struct{}{}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read service unit %q: %w", path, err)
	}
	for _, name := range requiredSupervisorSmokeProperties {
		if _, ok := seen[name]; !ok {
			return nil, fmt.Errorf("service unit lacks smoke-tested %s=", name)
		}
	}
	return properties, nil
}

func copyExecutable(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func stopSmokeSlot(backend *Backend, unit string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := backend.Stop(ctx, unit); err != nil {
		return backend.Wait(ctx, unit)
	}
	return nil
}

func stopSmokeSupervisor(unit string) error {
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	out, stopErr := runner.CommandContext(
		stopCtx,
		"systemctl",
		"stop",
		unit,
	).CombinedOutput()
	stopCancel()
	if stopErr == nil {
		return nil
	}

	observeCtx, observeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer observeCancel()
	state, observeErr := New(Options{}).isActive(observeCtx, unit)
	if observeErr != nil {
		return fmt.Errorf(
			"stop supervisor: %w: %s; confirm exit: %v",
			stopErr,
			tail(out),
			observeErr,
		)
	}
	if state == "inactive" || state == "failed" {
		return nil
	}
	return fmt.Errorf(
		"stop supervisor: %w: %s; active state %s",
		stopErr,
		tail(out),
		state,
	)
}

func cleanupSmoke(t *testing.T, root, supervisorUnit, slotUnit string) {
	t.Helper()
	if err := stopSmokeSupervisor(supervisorUnit); err != nil {
		t.Errorf("could not confirm supervisor exit; retained %s: %v", root, err)
		return
	}
	if err := stopSmokeSlot(New(Options{StopTimeout: 5 * time.Second}), slotUnit); err != nil {
		t.Errorf("could not confirm smoke unit exit; retained %s: %v", root, err)
		return
	}
	if err := os.RemoveAll(root); err != nil {
		t.Error(err)
	}
}
