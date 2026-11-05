package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"

	"github.com/nicogranuja/durable-mcp/internal/github"
	"github.com/nicogranuja/durable-mcp/internal/report"
)

// workerStopTimeout is how long a stopping worker lets running activities
// finish. It matches the GitHub client's request timeout, so a page that is
// being fetched can complete instead of being retried elsewhere.
const workerStopTimeout = github.DefaultTimeout

// workerConfig is the worker's configuration, from flags and the
// environment.
type workerConfig struct {
	temporalAddress   string
	temporalNamespace string
	taskQueue         string
	githubAPIURL      string
	githubToken       string
}

// parseWorkerFlags reads the worker's configuration. Each flag defaults to
// an environment variable, and then to a value that works with a local
// Temporal development server. The GitHub token is read only from the
// environment, so it does not show up in process listings or shell history.
func parseWorkerFlags(args []string, getenv func(string) string, stderr io.Writer) (workerConfig, error) {
	envOr := func(key, fallback string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return fallback
	}

	cfg := workerConfig{githubToken: getenv("GITHUB_TOKEN")}
	fs := flag.NewFlagSet("durable-mcp worker", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&cfg.temporalAddress, "temporal-address", envOr("TEMPORAL_ADDRESS", client.DefaultHostPort),
		"Temporal frontend `host:port` (env TEMPORAL_ADDRESS)")
	fs.StringVar(&cfg.temporalNamespace, "temporal-namespace", envOr("TEMPORAL_NAMESPACE", client.DefaultNamespace),
		"Temporal `namespace` (env TEMPORAL_NAMESPACE)")
	fs.StringVar(&cfg.taskQueue, "task-queue", envOr("DURABLE_MCP_TASK_QUEUE", report.DefaultTaskQueue),
		"Temporal task `queue` to poll (env DURABLE_MCP_TASK_QUEUE)")
	fs.StringVar(&cfg.githubAPIURL, "github-api-url", envOr("GITHUB_API_URL", github.DefaultBaseURL),
		"GitHub REST API `URL`, for GitHub Enterprise Server (env GITHUB_API_URL)")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `durable-mcp worker runs the Temporal worker for repository reports. It
polls a task queue for the RepositoryReport workflow and its activities,
and stops on SIGINT or SIGTERM.

Usage:
  durable-mcp worker [flags]

Environment:
  GITHUB_TOKEN  GitHub token for API requests. Without one, GitHub allows
                60 requests per hour and only public repositories.

Flags:
`)
		fs.PrintDefaults()
	}

	// Parse reports its own errors, followed by the usage message; report
	// the checks below the same way.
	if err := fs.Parse(args); err != nil {
		return workerConfig{}, err
	}
	var err error
	switch {
	case fs.NArg() > 0:
		err = fmt.Errorf("unexpected arguments: %q", fs.Args())
	case cfg.temporalAddress == "":
		err = errors.New("-temporal-address must not be empty")
	case cfg.temporalNamespace == "":
		err = errors.New("-temporal-namespace must not be empty")
	case cfg.taskQueue == "":
		err = errors.New("-task-queue must not be empty")
	}
	if err != nil {
		fmt.Fprintln(fs.Output(), err)
		fs.Usage()
		return workerConfig{}, err
	}
	return cfg, nil
}

// runWorker runs the worker subcommand until ctx is canceled and returns
// the process exit code.
func runWorker(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer) int {
	cfg, err := parseWorkerFlags(args, getenv, stderr)
	switch {
	case errors.Is(err, flag.ErrHelp):
		return 0
	case err != nil:
		return 2 // already reported
	}

	logger := slog.New(slog.NewTextHandler(stderr, nil))
	if err := serveWorker(ctx, cfg, logger); err != nil {
		logger.Error("Worker failed", "Error", err)
		return 1
	}
	return 0
}

// serveWorker connects to Temporal and runs a worker for the report
// workflow and activities until ctx is canceled.
func serveWorker(ctx context.Context, cfg workerConfig, logger *slog.Logger) error {
	gh, err := github.New(cfg.githubToken, github.WithBaseURL(cfg.githubAPIURL))
	if err != nil {
		return err
	}
	if cfg.githubToken == "" {
		logger.Warn("GITHUB_TOKEN is not set: GitHub allows 60 unauthenticated requests per hour, for public repositories only")
	}

	c, err := client.DialContext(ctx, client.Options{
		HostPort:  cfg.temporalAddress,
		Namespace: cfg.temporalNamespace,
		Logger:    log.NewStructuredLogger(logger),
	})
	if err != nil {
		return fmt.Errorf("connect to Temporal at %s: %w", cfg.temporalAddress, err)
	}
	defer c.Close()

	w := worker.New(c, cfg.taskQueue, worker.Options{WorkerStopTimeout: workerStopTimeout})
	report.Register(w, report.NewActivities(gh))

	// Run stops the worker when interrupt is closed, which happens when ctx
	// is canceled by SIGINT or SIGTERM.
	interrupt := make(chan any)
	stop := context.AfterFunc(ctx, func() { close(interrupt) })
	defer stop()
	return w.Run(interrupt)
}
