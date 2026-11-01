// Command durable-mcp is an MCP server that runs long-running agent tools as
// durable Temporal workflows.
//
// The project is a work in progress. The MCP server and the Temporal worker
// are not implemented yet; for now the command only reports its version.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run parses args and returns the process exit code. It takes its arguments
// and writers explicitly so it can be tested without touching os state.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("durable-mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `durable-mcp runs long-running agent tools as durable Temporal workflows.

This is an early work in progress: the MCP server and worker are not
implemented yet.

Usage:
  durable-mcp -version

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
