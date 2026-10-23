// Command hephaestus forges release artifacts into reliable distributions.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/magicdrive/hephaestus/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Main(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
