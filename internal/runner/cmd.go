package runner

import (
	"os/exec"
)

// JITScript returns the shell command that starts the official runner with
// the JIT config read from jitPath. The shell expands "$(cat ...)" at exec
// time, so the encoded value never sits in a long-lived argv. The path is
// single-quoted so daemon-controlled tmpfs paths with spaces still work.
func JITScript(jitPath string) string {
	return "exec ./run.sh --jitconfig \"$(cat '" + jitPath + "')\""
}

// BuildCommand returns the exec.Cmd for spec: a POSIX shell that execs
// ./run.sh in the slot directory with the JIT config read from the JIT
// file. The process runs in its own process group; see process.Backend.
func BuildCommand(spec Spec) *exec.Cmd {
	cmd := exec.Command("/bin/sh", "-c", JITScript(spec.JITPath))
	cmd.Dir = spec.SlotDir
	return cmd
}
