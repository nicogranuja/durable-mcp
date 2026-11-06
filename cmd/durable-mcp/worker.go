package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"

	"github.com/nicogranuja/durable-mcp/internal/github"
	"github.com/nicogranuja/durable-mcp/internal/report"
)

const (
	// maxConcurrentActivities caps how many GitHub requests one worker makes
	// at once. Every report on a worker shares its token, and GitHub counts
	// concurrent requests from one token against its secondary rate limit.
	// A report fetches its pages one at a time; this keeps many reports at
	// once from adding up to a burst.
	maxConcurrentActivities = 4

	// workerStopTimeout is how long a stopping worker lets running
	// activities finish. It matches the activities' start-to-close timeout,
	// so a page being fetched can complete instead of being retried
	// elsewhere.
	workerStopTimeout = time.Minute
)

// workerConfig is the worker's configuration, from flags and the
// environment.
type workerConfig struct {
	temporal     temporalConfig
	githubAPIURL string
	githubToken  string
}

// parseWorkerFlags reads the worker's configuration. Each flag defaults to
// an environment variable, and then to a value that works with a local
// Temporal development server. The GitHub token is read only from the
// environment, so it does not show up in process listings or shell history.
func parseWorkerFlags(args []string, getenv func(string) string, stderr io.Writer) (workerConfig, error) {
	cfg := workerConfig{githubToken: getenv("GITHUB_TOKEN")}
	fs := flag.NewFlagSet("durable-mcp worker", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg.temporal.register(fs, getenv, "poll")
	fs.StringVar(&cfg.githubAPIURL, "github-api-url", envOr(getenv, "GITHUB_API_URL", github.DefaultBaseURL),
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
	err := cfg.temporal.check()
	if fs.NArg() > 0 {
		err = fmt.Errorf("unexpected arguments: %q", fs.Args())
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
		HostPort:  cfg.temporal.address,
		Namespace: cfg.temporal.namespace,
		Logger:    log.NewStructuredLogger(logger),
	})
	if err != nil {
		return fmt.Errorf("connect to Temporal at %s: %w", cfg.temporal.address, err)
	}
	defer c.Close()

	w := worker.New(c, cfg.temporal.taskQueue, worker.Options{
		MaxConcurrentActivityExecutionSize: maxConcurrentActivities,
		WorkerStopTimeout:                  workerStopTimeout,
	})
	report.Register(w, report.NewActivities(gh))

	// Run stops the worker when interrupt is closed, which happens when ctx
	// is canceled by SIGINT or SIGTERM.
	interrupt := make(chan any)
	stop := context.AfterFunc(ctx, func() { close(interrupt) })
	defer stop()
	return w.Run(interrupt)
}
