package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// JITScript reads the credential from the first positional argument. The
// official runner still receives the value in argv for its process lifetime.
func JITScript() string {
	return `jit=$(cat -- "$1") || exit; exec ./run.sh --jitconfig "$jit"`
}

// CredentialScript reads systemd's private, per-unit credential copy.
func CredentialScript() string {
	return `jit=$(cat "$CREDENTIALS_DIRECTORY/jit") || exit; exec ./run.sh --jitconfig "$jit"`
}

// BuildCommand keeps paths out of shell source and secrets out of the
// supervisor's command line. The runner itself requires --jitconfig on argv.
func BuildCommand(spec Spec) *exec.Cmd {
	cmd := exec.Command("/bin/sh", "-c", JITScript(), "--", spec.JITPath)
	cmd.Dir = spec.SlotDir
	return cmd
}

// CommandContext bounds command cancellation even if a subprocess inherits
// its parent's output pipes. Each command owns a separate process group.
func CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
	return cmd
}
