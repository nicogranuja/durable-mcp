// Command durable-mcp is an MCP server that runs long-running agent tools as
// durable Temporal workflows.
//
// "durable-mcp serve" runs the MCP server, over stdio or streamable HTTP,
// and "durable-mcp worker" runs the Temporal worker that does the work.
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
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run parses args and returns the process exit code. It takes its context,
// arguments, environment, and standard streams explicitly so it can be
// tested without touching os state. Canceling ctx stops a running
// subcommand.
func run(ctx context.Context, args []string, getenv func(string) string, stdin io.ReadCloser, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "serve":
			return runServe(ctx, args[1:], getenv, stdin, stdout, stderr)
		case "worker":
			return runWorker(ctx, args[1:], getenv, stderr)
		}
	}

	fs := flag.NewFlagSet("durable-mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `durable-mcp runs long-running agent tools as durable Temporal workflows.

Usage:
  durable-mcp serve [flags]    run the MCP server, over stdio or streamable HTTP
  durable-mcp worker [flags]   run the Temporal worker for repository reports
  durable-mcp -version

Run "durable-mcp serve -h" or "durable-mcp worker -h" for their flags.

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
