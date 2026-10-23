package hephaestus

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// Execute runs the command line with os.Args and exits the process.
func Execute(version string) {
	Version = version
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := Main(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
