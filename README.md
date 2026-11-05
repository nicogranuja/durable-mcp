# durable-mcp

[![CI](https://github.com/nicogranuja/durable-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/nicogranuja/durable-mcp/actions/workflows/ci.yml)

durable-mcp is an [MCP](https://modelcontextprotocol.io) server that lets AI agents run long-running jobs reliably.
Instead of holding a tool call open for minutes, a tool starts a job and returns a job ID right away.
[Temporal](https://temporal.io) runs the work durably, with retries, heartbeats, and cancellation, and the agent checks
the job's status and fetches the result when it is ready. The example job is a GitHub repository report: issues, pull
requests, and commit activity, fetched through the GitHub REST API with pagination and rate-limit handling.

## Status: work in progress

- [x] **Foundation:** repository layout, Makefile, CI (race-enabled tests, golangci-lint, govulncheck), and a
      hand-written GitHub REST client with token auth, Link-header pagination, typed errors for primary and secondary
      rate limits, timeouts, and table-driven `httptest` tests.
- [x] **Durable jobs:** the report as a Temporal workflow and activities (one page per activity, heartbeats, retry
      policies), a worker command, and workflow tests.
- [ ] **MCP server:** `start_report`, `job_status`, `job_result`, and `cancel_job` tools on a Temporal client, over
      stdio and streamable HTTP.
- [ ] **Hardening:** idempotent starts, timeouts and cancellation end to end, input validation, an error taxonomy
      agents can act on, OpenTelemetry metrics and traces, a Grafana dashboard, and bearer-token auth.
- [ ] **Docs and release:** architecture and design notes, an operations runbook, a Dockerfile, and v0.1.0.

## Layout

```
cmd/durable-mcp/   command entry point: the Temporal worker for now
internal/github/   GitHub REST API client
internal/report/   the repository report as a Temporal workflow and activities
```

The design notes live in package documentation: the [GitHub client](internal/github/doc.go), including why it returns
rate-limit errors instead of sleeping, and the [report workflow](internal/report/doc.go), including its retry policy,
timeouts, and history size.

## Architecture so far

```
durable-mcp worker ── polls the "durable-mcp" task queue on Temporal
  └─ RepositoryReport workflow          adds up page summaries, answers the "progress" query
       ├─ FetchRepository               one request; fails fast on 401 or 404
       ├─ FetchIssuesPage        x N    one page per activity, each starting at the
       ├─ FetchPullRequestsPage  x N    previous page's cursor
       └─ FetchCommitsPage       x N
            └─ internal/github ── GitHub REST API
```

- **The workflow decides, activities do the I/O.** Each activity fetches exactly one page, so a failure repeats one page,
  not the whole report. The workflow only adds up results and picks the next page, deterministically, so Temporal can
  replay it after a crash.
- **Small history.** An activity returns a summary of its page (counts, merge times, commits per week and per author),
  never the raw issues, so workflow history grows with the number of pages, not with the data.
- **Rate limits are durable waits.** A rate-limited activity fails with a retry delay set to GitHub's reset time. The
  Temporal server holds the wait, so no worker is tied up and a restart loses nothing. Missing repositories, bad tokens,
  and similar 4xx errors are not retried; 5xx and network errors are, with exponential backoff.
- **Progress and cancellation.** The `progress` query reports the phase and pages fetched so far. Canceling the workflow
  cancels the activity in flight, which notices through its heartbeat and aborts its request.

## Running the worker locally

A Temporal server is only needed to run the worker; the tests don't use one. The
[Temporal CLI](https://docs.temporal.io/cli) can start a local development server, with a web UI at
http://localhost:8233:

```sh
temporal server start-dev
```

Then, in another terminal, run the worker. The token is optional, but without one GitHub allows only 60 requests per
hour, for public repositories:

```sh
export GITHUB_TOKEN=...   # a token that can read the repositories you report on
go run ./cmd/durable-mcp worker
```

`durable-mcp worker -h` lists the flags for the Temporal address, namespace, and task queue, and the GitHub API URL for
GitHub Enterprise Server. Until the MCP tools exist, start a report and follow it with the CLI:

```sh
temporal workflow start --task-queue durable-mcp --type RepositoryReport \
  --workflow-id report-sdk-go --input '{"owner": "temporalio", "repo": "sdk-go"}'
temporal workflow query --workflow-id report-sdk-go --name progress
temporal workflow result --workflow-id report-sdk-go
```

## Running the tests

You need Go 1.26 or newer. With the default `GOTOOLCHAIN=auto`, an older `go` command downloads a suitable toolchain.

```sh
make test            # go test -race ./...
make lint            # golangci-lint, at the version CI uses
make workflowcheck   # static check that workflow code is deterministic
make vuln            # govulncheck
make                 # vet, test, lint, and workflowcheck
```

The tests run against local `httptest` servers and never call the real GitHub API, so no token is needed. The workflow
tests use the Temporal SDK's test environment, which skips ahead through timers and retry delays, so they need no
Temporal server and a half-hour rate-limit wait takes milliseconds.

## How this was built

This project is built with AI coding agents (Claude Code) under my direction: I set the goals, architecture, and
priorities, and the agents write and test the code.

## License

[MIT](LICENSE)
