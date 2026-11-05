// Command ibc runs an ibc node: the HTTP API, consensus and contract
// execution in one process, with its data in a Pebble database.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ouripoupko/ibc-go/node"
)

func main() {
	addr := flag.String("addr", envOr("IBC_ADDR", ":2611"), "address to serve the API on (env IBC_ADDR)")
	dataDir := flag.String("data", envOr("IBC_DATA", "data"), "directory of the node's database (env IBC_DATA)")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := run(*addr, *dataDir); err != nil {
		fmt.Fprintln(os.Stderr, "ibc:", err)
		os.Exit(1)
	}
}

func run(addr, dataDir string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	n, err := node.New(node.Config{DataDir: dataDir})
	if err != nil {
		return err
	}
	slog.Info("ibc node starting", "data", dataDir)
	return n.ListenAndServe(ctx, addr)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
