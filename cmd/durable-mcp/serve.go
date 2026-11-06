package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/log"

	"github.com/nicogranuja/durable-mcp/internal/mcpserver"
)

// Transports that serve can use.
const (
	transportStdio = "stdio"
	transportHTTP  = "http"
)

const (
	// defaultHTTPAddr is where the HTTP transport listens by default:
	// loopback only, because it has no authentication yet.
	defaultHTTPAddr = "127.0.0.1:8080"

	// mcpPath is the URL path of the streamable HTTP endpoint.
	mcpPath = "/mcp"

	// httpShutdownTimeout is how long a stopping HTTP server lets requests
	// in flight finish. Tool calls return within seconds.
	httpShutdownTimeout = 10 * time.Second

	// healthCheckTimeout bounds the Temporal check at startup.
	healthCheckTimeout = 5 * time.Second
)

// serveConfig is the MCP server's configuration, from flags and the
// environment.
type serveConfig struct {
	temporal  temporalConfig
	transport string
	httpAddr  string
}

// parseServeFlags reads the MCP server's configuration, in the same way as
// parseWorkerFlags.
func parseServeFlags(args []string, getenv func(string) string, stderr io.Writer) (serveConfig, error) {
	var cfg serveConfig
	fs := flag.NewFlagSet("durable-mcp serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg.temporal.register(fs, getenv, "start reports on")
	fs.StringVar(&cfg.transport, "transport", envOr(getenv, "DURABLE_MCP_TRANSPORT", transportStdio),
		"MCP `transport`: stdio, or http for streamable HTTP (env DURABLE_MCP_TRANSPORT)")
	fs.StringVar(&cfg.httpAddr, "http-addr", envOr(getenv, "DURABLE_MCP_HTTP_ADDR", defaultHTTPAddr),
		"`host:port` for the http transport to listen on; the host must be a loopback IP address (env DURABLE_MCP_HTTP_ADDR)")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `durable-mcp serve runs the MCP server, whose tools start repository
reports as Temporal workflows and report on them. It needs a Temporal
server and a running "durable-mcp worker" to do the work, and stops on
SIGINT or SIGTERM, or, with the stdio transport, when stdin closes.

The http transport serves streamable HTTP at http://HOST:PORT`+mcpPath+`. It has no
authentication yet, so it only listens on loopback IP addresses.

Usage:
  durable-mcp serve [flags]

Flags:
`)
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return serveConfig{}, err
	}
	err := cfg.temporal.check()
	switch {
	case fs.NArg() > 0:
		err = fmt.Errorf("unexpected arguments: %q", fs.Args())
	case cfg.transport != transportStdio && cfg.transport != transportHTTP:
		err = fmt.Errorf("-transport must be %s or %s, got %q", transportStdio, transportHTTP, cfg.transport)
	case cfg.transport == transportHTTP:
		if lerr := checkLoopback(cfg.httpAddr); lerr != nil {
			err = lerr
		}
	}
	if err != nil {
		fmt.Fprintln(fs.Output(), err)
		fs.Usage()
		return serveConfig{}, err
	}
	return cfg, nil
}

// checkLoopback reports an error unless addr is a loopback host and port.
// Until the HTTP transport has authentication, anyone who can reach it can
// start and cancel jobs, so it must not listen beyond this machine.
func checkLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("-http-addr %q is not host:port: %w", addr, err)
	}
	// An IP literal, not a name such as localhost: a name is only as
	// loopback as the resolver says.
	if ip, err := netip.ParseAddr(host); err == nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("-http-addr %q is not a loopback address: the HTTP transport has no authentication yet, "+
		"so it only listens on a loopback IP address, such as 127.0.0.1 or [::1]", addr)
}

// runServe runs the serve subcommand until ctx is canceled, or stdin
// closes with the stdio transport, and returns the process exit code.
func runServe(ctx context.Context, args []string, getenv func(string) string, stdin io.ReadCloser, stdout, stderr io.Writer) int {
	cfg, err := parseServeFlags(args, getenv, stderr)
	switch {
	case errors.Is(err, flag.ErrHelp):
		return 0
	case err != nil:
		return 2 // already reported
	}

	// Logs go to stderr: with the stdio transport, stdout carries the
	// protocol.
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	if err := serveMCP(ctx, cfg, stdin, stdout, logger); err != nil {
		logger.Error("MCP server failed", "Error", err)
		return 1
	}
	return 0
}

// serveMCP runs the MCP server on the configured transport.
func serveMCP(ctx context.Context, cfg serveConfig, stdin io.ReadCloser, stdout io.Writer, logger *slog.Logger) error {
	// A lazy client connects on first use, so the server starts, and lists
	// its tools, even while Temporal is down; the tools then say that
	// Temporal is unreachable, and work once it is back.
	c, err := client.NewLazyClient(client.Options{
		HostPort:  cfg.temporal.address,
		Namespace: cfg.temporal.namespace,
		Logger:    log.NewStructuredLogger(logger),
	})
	if err != nil {
		return fmt.Errorf("configure the Temporal client for %s: %w", cfg.temporal.address, err)
	}
	defer c.Close()
	checkCtx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	if _, err := c.CheckHealth(checkCtx, &client.CheckHealthRequest{}); err != nil {
		logger.Warn("Temporal is not reachable yet; tools will say so until it is",
			"Address", cfg.temporal.address, "Error", err)
	}
	cancel()

	server := mcpserver.New(c, mcpserver.Options{TaskQueue: cfg.temporal.taskQueue, Version: version, Logger: logger})
	if cfg.transport == transportHTTP {
		return serveHTTP(ctx, server, cfg.httpAddr, logger)
	}
	logger.Info("Serving MCP over stdio", "TaskQueue", cfg.temporal.taskQueue)
	err = server.Run(ctx, &mcp.IOTransport{Reader: stdin, Writer: nopWriteCloser{stdout}})
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return nil // stopped by a signal
	}
	return err
}

// nopWriteCloser is a Writer that does not close, so the transport does
// not close the process's stdout.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// serveHTTP serves server over streamable HTTP until ctx is canceled.
func serveHTTP(ctx context.Context, server *mcp.Server, addr string, logger *slog.Logger) error {
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler: httpHandler(server, logger),
		// No tool streams or takes long, and stateless mode has no
		// long-lived GET stream, so every request can be bounded.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      time.Minute,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	logger.Info("Serving MCP over streamable HTTP", "URL", "http://"+ln.Addr().String()+mcpPath)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), httpShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close() // drop the requests that did not finish in time
		return fmt.Errorf("shut down the HTTP server: %w", err)
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// httpHandler returns the HTTP handler for the streamable HTTP transport.
//
// The server is stateless: the tools keep no per-session state, so each
// request gets a temporary session, nothing is lost when the process
// restarts, and replicas need no session affinity. Responses are plain
// JSON, as no tool streams.
//
// Two checks protect a server on loopback from web pages in the user's
// browser: the SDK rejects requests whose Host header is not a loopback
// name (DNS rebinding), and CrossOriginProtection rejects cross-origin
// browser requests.
func httpHandler(server *mcp.Server, logger *slog.Logger) http.Handler {
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, Logger: logger})
	mux := http.NewServeMux()
	mux.Handle(mcpPath, http.NewCrossOriginProtection().Handler(mcpHandler))
	return mux
}
