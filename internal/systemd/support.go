package systemd

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/0xinterface/tentacles/internal/runner"
)

// CheckSupport verifies the systemd-run flag needed to preserve the static
// credential shell without manager-side environment expansion.
func CheckSupport(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := runner.CommandContext(ctx, "systemd-run", "--version").Output()
	if err != nil {
		return fmt.Errorf("systemd version: %w", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 || fields[0] != "systemd" {
		return fmt.Errorf("unrecognized systemd version")
	}
	version, err := strconv.Atoi(fields[1])
	if err != nil || version < 254 {
		return fmt.Errorf("systemd >=254 is required for private credential command delivery (found %s)", fields[1])
	}
	return nil
}
