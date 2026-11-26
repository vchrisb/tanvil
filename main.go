// Command tanvil manages a Talos Linux cluster through the Talos and Kubernetes Go APIs.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/vchrisb/tanvil/internal/cli"
	"github.com/vchrisb/tanvil/internal/cluster"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := cli.NewRootCommand().ExecuteContext(ctx)

	switch {
	case err == nil:
	case errors.Is(err, cluster.ErrCancelled):
		fmt.Fprintln(os.Stderr, "Operation cancelled by user.")
	case errors.Is(err, cluster.ErrChanges):
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	case errors.Is(err, cluster.ErrDrift):
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	default:
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
