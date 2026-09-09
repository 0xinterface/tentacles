// Command tentacles is the GitHub Actions host runner supervisor.
// It owns one runner scale set and keeps ephemeral, JIT-configured
// official runner processes alive on this host (see docs/spike.md and
// README.md). No business logic lives here.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/hkust/tentacles/internal/app"
	"github.com/hkust/tentacles/internal/version"
)

func main() {
	configPath := flag.String("config", "/etc/tentacles/config.yaml", "path to config.yaml")
	dryRun := flag.Bool("dry-run", false, "validate the config and exit 0")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		os.Exit(0)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := app.Run(ctx, app.Options{ConfigPath: *configPath, DryRun: *dryRun}); err != nil && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "tentacles: %v\n", err)
		os.Exit(1)
	}
}
