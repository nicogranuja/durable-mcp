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
- [x] **MCP server:** `start_report`, `job_status`, `job_result`, and `cancel_job` tools on a Temporal client, over
      stdio and streamable HTTP, with a "use it from Claude Code" guide.
- [ ] **Hardening:** idempotent starts, timeouts and cancellation end to end, input validation, an error taxonomy
      agents can act on, OpenTelemetry metrics and traces, a Grafana dashboard, and bearer-token auth.
- [ ] **Docs and release:** architecture and design notes, an operations runbook, a Dockerfile, and v0.1.0.

## Layout

```
cmd/durable-mcp/      command entry point: "serve" runs the MCP server, "worker" the Temporal worker
internal/mcpserver/   the MCP tools, on an interface over the Temporal client
internal/report/      the repository report as a Temporal workflow and activities
internal/github/      GitHub REST API client
```

The design notes live in package documentation: the [MCP tools](internal/mcpserver/doc.go), including job IDs and
idempotency, the [report workflow](internal/report/doc.go), including its retry policy, timeouts, and history size, and
the [GitHub client](internal/github/doc.go), including why it returns rate-limit errors instead of sleeping.

## Architecture

```
AI agent (Claude Code, ...)
  │  MCP over stdio or streamable HTTP
  ▼
durable-mcp serve        start_report · job_status · job_result · cancel_job
  │  Temporal client: start, describe, query, get result, cancel
  ▼
Temporal server          holds every job's state, timers, and retries
  │  "durable-mcp" task queue
  ▼
durable-mcp worker
  └─ RepositoryReport workflow          adds up page summaries, answers the "progress" query
       ├─ FetchRepository               one request; fails fast on 401 or 404
       ├─ FetchIssuesPage        x N    one page per activity, each starting at the
       ├─ FetchPullRequestsPage  x N    previous page's cursor
       └─ FetchCommitsPage       x N
            └─ internal/github ── GitHub REST API
```

- **Tools return at once; Temporal does the waiting.** A report can take hours, so `start_report` returns a job ID right
  away, and the agent polls `job_status` and fetches `job_result`. The MCP server keeps no state: the job ID is the
  workflow ID, and everything else lives in Temporal, so the server can restart without losing a job.
- **Retries are safe.** An optional `idempotency_key` maps to a fixed job ID, and the workflow ID reuse policy
  (RejectDuplicate) refuses to start that ID twice, so a retried `start_report` returns the job it already started.
  Reusing a key for a different request is an error. Every job has a 24-hour execution timeout.
- **The workflow decides, activities do the I/O.** Each activity fetches exactly one page, so a failure repeats one page,
  not the whole report. The workflow only adds up results and picks the next page, deterministically, so Temporal can
  replay it after a crash.
- **Small history.** An activity returns a summary of its page (counts, merge times, commits per week and per author),
  never the raw issues, so workflow history grows with the number of pages, not with the data.
- **Rate limits are durable waits, and the agent can see them.** A rate-limited activity fails with a retry delay set to
  GitHub's reset time. The Temporal server holds the wait, so no worker is tied up and a restart loses nothing. That
  retry is not in the workflow's history or its progress query; it is in the pending activity, which `job_status` reads
  with `DescribeWorkflowExecution` and reports as "waiting for GitHub rate limit until ...". Missing repositories, bad
  tokens, and similar 4xx errors are not retried; 5xx and network errors are, with exponential backoff.
- **Progress and cancellation.** The `progress` query reports the phase and pages fetched so far. `cancel_job` cancels
  the workflow, which cancels the activity in flight; it notices through its heartbeat and aborts its request.

## Running it locally

You need a Temporal server, the worker, and the MCP server. The [Temporal CLI](https://docs.temporal.io/cli) starts a
local development server, with a web UI at http://localhost:8233:

```sh
temporal server start-dev
```

In another terminal, run the worker. The token is optional, but without one GitHub allows only 60 requests per hour,
for public repositories. The worker is the only part that needs it; the MCP server never sees it.

```sh
export GITHUB_TOKEN=...   # a token that can read the repositories you report on
go run ./cmd/durable-mcp worker
```

Then build the MCP server, which your MCP host (such as Claude Code, below) starts over stdio, or which you run yourself
for streamable HTTP:

```sh
make build                                  # writes bin/durable-mcp
bin/durable-mcp serve -transport http       # serves http://127.0.0.1:8080/mcp
```

`durable-mcp serve -h` and `durable-mcp worker -h` list the flags for the Temporal address, namespace, and task queue
(the same for both, with the same environment variables), the transport, and, for the worker, the GitHub API URL for
GitHub Enterprise Server.

> [!WARNING]
> The HTTP transport has **no authentication yet** (bearer-token auth is the next milestone). Anyone who can reach it can
> start and cancel jobs, so it listens on `127.0.0.1` by default and refuses any address that is not loopback. It also
> rejects requests with a non-loopback `Host` header (DNS rebinding) and cross-origin browser requests, so web pages
> cannot reach it either.

## Use it from Claude Code

With the Temporal server and the worker running, add the server to Claude Code. Over stdio, Claude Code starts and stops
the server itself:

```sh
claude mcp add durable-mcp -- /path/to/durable-mcp/bin/durable-mcp serve
# with a non-default Temporal server:
claude mcp add durable-mcp -e TEMPORAL_ADDRESS=temporal.internal:7233 -- /path/to/durable-mcp/bin/durable-mcp serve
```

Or, with `bin/durable-mcp serve -transport http` running, over streamable HTTP:

```sh
claude mcp add --transport http durable-mcp http://127.0.0.1:8080/mcp
```

Add `--scope user` to make it available in every project. Then ask for a report. A conversation looks like this (the
tool calls are what Claude Code shows; the numbers are from a real run):

> **You:** Start a report on temporalio/sdk-go for the last 30 days.
>
> **Claude:** *calls `start_report` with `{"owner": "temporalio", "repo": "sdk-go", "idempotency_key": "4f9c…"}`*
> Started job `report-6gmqnxnfeb3bdg27poofcuoe4i`. I'll check on it.
>
> *calls `job_status`* → `running`, in phase `repository`.
>
> *calls `job_status`* a few seconds later → `completed`, after 2 issue, 2 pull request, and 1 commit pages.
> *calls `job_result`*
>
> **Claude:** In the last 30 days temporalio/sdk-go merged 65 of 99 new pull requests (median time to merge about 44
> hours), opened 11 issues and closed 12, and had 65 commits from 28 contributors…

On a worker without a token, a large report runs into GitHub's rate limit, and `job_status` says so. The agent can wait,
do something else, or cancel:

> *calls `job_status`* → "Running, in phase issues, with 58 issue, 0 pull request, and 0 commit pages fetched so far.
> Waiting for GitHub rate limit until 2026-11-06T07:40:20Z (in 59m4s); the job resumes on its own, so check back after
> that."
>
> **You:** Never mind, cancel it.
>
> *calls `cancel_job`* → "Cancellation requested; the job is stopping."

The job keeps running if Claude Code exits; ask about the job ID in a later session to pick it up again.

### Inspecting jobs with the Temporal CLI

Every job is a workflow whose ID is the job ID, so the Temporal CLI and web UI show it too. `describe` shows the pending
activity, with its attempt count, last failure, and next attempt time:

```sh
temporal workflow describe --workflow-id report-6gmqnxnfeb3bdg27poofcuoe4i
temporal workflow query --workflow-id report-6gmqnxnfeb3bdg27poofcuoe4i --name progress
temporal workflow result --workflow-id report-6gmqnxnfeb3bdg27poofcuoe4i
```

You can also start a report without the MCP server:

```sh
temporal workflow start --task-queue durable-mcp --type RepositoryReport \
  --workflow-id report-sdk-go --input '{"owner": "temporalio", "repo": "sdk-go"}'
```

The MCP tools only accept job IDs that `start_report` issues, so they will not show a workflow started this way.

## Running the tests

You need Go 1.26 or newer. With the default `GOTOOLCHAIN=auto`, an older `go` command downloads a suitable toolchain.

```sh
make test            # go test -race ./...
make lint            # golangci-lint, at the version CI uses
make workflowcheck   # static check that workflow code is deterministic
make vuln            # govulncheck
make                 # vet, test, lint, and workflowcheck
```

No test needs a Temporal server, a GitHub token, or the network:

- The GitHub client tests run against local `httptest` servers.
- The workflow tests use the Temporal SDK's test environment, which skips ahead through timers and retry delays, so a
  half-hour rate-limit wait takes milliseconds. A replay test runs the workflow against a
  [history](internal/report/testdata/repository_report_history.json) recorded from a real run, to catch changes that
  would break running workflows.
- The MCP tool tests use a fake Temporal client, and protocol tests connect a real MCP client to the server over the
  SDK's in-memory transport, list the tools, check their schemas, and call each one. The command tests run the server
  over stdio pipes and streamable HTTP.

## How this was built

This project is built with AI coding agents (Claude Code) under my direction: I set the goals, architecture, and
priorities, and the agents write and test the code.

## License

[MIT](LICENSE)
