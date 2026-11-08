package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/nicogranuja/durable-mcp/internal/auth"
)

// runToken runs the token subcommand: it mints a token for a caller and
// prints it with the caller's entry for the server's token list, and
// returns the process exit code.
//
// The token goes to the caller, and only the entry, which holds the
// token's hash, goes to the server. Nothing is stored.
func runToken(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("durable-mcp token", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `durable-mcp token mints a bearer token for the caller NAME, and prints it
with the caller's entry for the MCP server's auth tokens:

  token: dmcp_...        give this to the caller; it is not stored anywhere
  entry: NAME:SHA256     add this line to the -auth-tokens-file of "durable-mcp serve"

The server keeps only the entry, so a leaked tokens file lets no one in.
To rotate a caller's token, mint a new one for the same name, replace the
caller's entry, and restart the server.

Usage:
  durable-mcp token NAME
`)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "want exactly one caller NAME")
		fs.Usage()
		return 2
	}
	name := fs.Arg(0)
	if err := auth.ValidateName(name); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	token := auth.NewToken()
	fmt.Fprintf(stdout, "token: %s\nentry: %s\n", token, auth.Entry(name, token))
	return 0
}
