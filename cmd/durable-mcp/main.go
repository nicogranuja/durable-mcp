// Command durable-mcp is an MCP server that runs long-running agent tools as
// durable Temporal workflows.
//
// The project is a work in progress. "durable-mcp worker" runs the Temporal
// worker for repository reports; the MCP server is not implemented yet.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run parses args and returns the process exit code. It takes its context,
// arguments, environment, and writers explicitly so it can be tested
// without touching os state. Canceling ctx stops a running subcommand.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "worker" {
		return runWorker(ctx, args[1:], getenv, stderr)
	}

	fs := flag.NewFlagSet("durable-mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `durable-mcp runs long-running agent tools as durable Temporal workflows.

This is an early work in progress: the MCP server is not implemented yet.

Usage:
  durable-mcp worker [flags]   run the Temporal worker for repository reports
  durable-mcp -version

Run "durable-mcp worker -h" for the worker's flags.

Flags:
`)
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if *showVersion {
		fmt.Fprintf(stdout, "durable-mcp %s\n", version)
		return 0
	}

	fs.Usage()
	return 2
}
