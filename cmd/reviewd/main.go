package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/faustbrian/reviewd/internal/cli"
)

var version = "0.1.0-dev"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr, cli.Dependencies{Version: version})
	cancel()
	os.Exit(code)
}
