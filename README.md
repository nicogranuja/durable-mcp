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
- [x] **Hardening:** idempotent starts, timeouts and cancellation end to end, input validation, an error taxonomy
      agents can act on, OpenTelemetry metrics and traces, a Grafana dashboard, and bearer-token auth.
- [ ] **Docs and release:** architecture and design notes, an operations runbook, a Dockerfile, and v0.1.0.

## Layout

```
cmd/durable-mcp/      command entry point: "serve" runs the MCP server, "worker" the Temporal worker
internal/mcpserver/   the MCP tools, on an interface over the Temporal client
internal/report/      the repository report as a Temporal workflow and activities
internal/github/      GitHub REST API client
internal/telemetry/   OpenTelemetry metrics setup: OTLP, Prometheus, and the Temporal SDK handler
deploy/grafana/       Grafana dashboard for the Prometheus metrics
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
       ├─ report.FetchRepository               one request; fails fast on 401 or 404
       ├─ report.FetchIssuesPage        x N    one page per activity, each starting at the
       ├─ report.FetchPullRequestsPage  x N    previous page's cursor
       └─ report.FetchCommitsPage       x N
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
  tokens, similar 4xx errors, and other failures that would repeat (a next-page link or redirect off the API) are not
  retried; 5xx and network errors are, with exponential backoff. Each failure has one type, such as `report.NotFound`,
  and `job_result` tells the agent whether a new report could succeed and what to do next.
- **Progress and cancellation.** The `progress` query reports the phase and pages fetched so far. `cancel_job` cancels
  the workflow, which cancels the activity in flight; it notices through its heartbeat and aborts its request.

## Timeouts

Every wait has a bound, and each layer's bound fits inside the one above it, so a slow or unreachable dependency turns
into an answer the agent can act on instead of a hung call.

| Layer | Setting | Value | Why |
|---|---|---|---|
| HTTP server | read header | 10s | A client that sends headers slowly cannot hold a connection; the token check needs them first. |
| | read | 30s | The whole request, body included. MCP requests are small. |
| | write | 1m | From the end of the headers to the end of the response, so it must outlast the tool call deadline. |
| | idle | 2m | How long a keep-alive connection waits for its next request. |
| | shutdown | 10s | On SIGINT or SIGTERM, requests in flight get this long to finish; any still running are cut off. |
| MCP server | tool call deadline | 30s | The whole tool call. `cancel_job` makes up to three Temporal calls of up to 10s each. Set with `-tool-timeout` (env `DURABLE_MCP_TOOL_TIMEOUT`), from 1s to 50s, which leaves 10s of the write timeout for the answer. |
| Temporal client | each call from a tool | 10s | Start, describe, get result, and cancel. A call that runs out of time becomes a tool error that says what is safe to retry. |
| | progress query | 5s | A query needs a worker to answer it; without one, `job_status` answers without progress instead of waiting. |
| | health check at startup | 5s | `serve` starts anyway, and its tools say Temporal is unreachable until it is back. |
| | GetSystemInfo on connect | 5s | The Temporal SDK's default, for the call the worker makes when it connects. |
| GitHub client | each request | 30s | Includes reading the body. |
| | response size | 10 MiB | A page of 100 issues is well under 1 MiB; a larger body fails instead of filling memory. |
| Report activities | start-to-close | 1m | One attempt at one page; longer than the GitHub timeout, so the client's more specific error comes first. |
| | heartbeat | 10s | Notices a lost worker sooner, and bounds how long a cancellation takes to reach a running request. |
| | schedule-to-close | 2h, at most 10 attempts | A page's whole budget, retries included; it outlasts GitHub's longest normal rate-limit reset, an hour. |
| Report job | execution timeout | 24h | The slowest legitimate report, on a worker without a GitHub token, takes about ten hours. |
| Worker | stop timeout | 1m | Matches start-to-close, so a page being fetched can finish instead of being retried elsewhere. |

The first SIGINT or SIGTERM starts a graceful stop; a second one ends the process at once. When running under a
supervisor or in a container, give it a stop grace period of at least 70 seconds (for example, `docker stop -t 70`, or
`terminationGracePeriodSeconds: 70` on Kubernetes), so that a stopping worker gets its full minute before it is killed.

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

Without auth tokens, anyone who can reach the HTTP transport can start and cancel jobs, so it listens on `127.0.0.1` by
default and refuses any address that is not a loopback IP. It also rejects requests with a non-loopback `Host` header
(DNS rebinding) and cross-origin browser requests, so web pages cannot reach it either. To accept callers from other
machines, configure tokens.

### Authentication

The HTTP transport can require a bearer token on every request. Each caller has a name and its own token, and the server
keeps only the SHA-256 of each token, so a leaked configuration lets no one in. Mint a token for each caller:

```sh
bin/durable-mcp token alice
# token: dmcp_...           give this to alice; it is not stored anywhere
# entry: alice:3f2a...      the line for the server's tokens file
```

Put the `entry:` values in a file, one per line (`#` starts a comment), and start the server with it. With tokens, the
server may listen beyond loopback:

```sh
bin/durable-mcp serve -transport http -http-addr 0.0.0.0:8080 -auth-tokens-file /etc/durable-mcp/tokens
```

The file can also come from `DURABLE_MCP_AUTH_TOKENS_FILE`, or the entries from `DURABLE_MCP_AUTH_TOKENS`,
comma-separated; setting both is an error. The stdio transport ignores tokens: its only caller is the MCP host that
started it. Callers send the token in the `Authorization` header:

```sh
curl -sS http://mcp.example.internal:8080/mcp \
  -H "Authorization: Bearer $DURABLE_MCP_TOKEN" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```

A request without a valid token gets `401 Unauthorized` with a `WWW-Authenticate: Bearer` challenge. Rejections are
logged at most once every 10 seconds, with a count of the ones left out, and never with the token. To rotate a token,
mint a new one for the same name, replace that caller's entry, and restart the server.

Jobs are private to the caller that started them. The caller's name is part of the job ID for an idempotency key, so two
callers with the same key get two jobs, and it is stored in the job's memo: `job_status`, `job_result`, and `cancel_job`
answer another caller's job exactly as they answer a job ID that does not exist. Jobs started over stdio, or over HTTP
without tokens, belong to no caller and are visible only there. Ownership follows the name, so rotating a token keeps
the caller's jobs. With tokens, the `Host` check is off, since a DNS-rebinding page has no token, so a TLS proxy on the
same machine may forward to `127.0.0.1` with the public `Host` header.

> [!WARNING]
> The transport is plain HTTP, so tokens travel unencrypted, and the server logs a warning when it listens beyond
> loopback. Beyond a single machine, put it behind a TLS-terminating proxy or keep it on a private network.

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
# with auth tokens:
claude mcp add --transport http durable-mcp https://mcp.example.internal/mcp --header "Authorization: Bearer $DURABLE_MCP_TOKEN"
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
> *calls `job_status`* a few seconds later → `completed`. *calls `job_result`*
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

## Observability

Both commands record OpenTelemetry metrics, the Temporal SDK's own included, and traces. Telemetry is off by default: a
plain run makes no network calls and opens no extra port. Turn metrics on either or both ways:

- **Prometheus (pull):** `-metrics-addr host:port` (env `DURABLE_MCP_METRICS_ADDR`) serves `/metrics` from a private
  registry. A bare `:9464` listens on `127.0.0.1`; name the host, such as `0.0.0.0:9464`, to listen beyond loopback.
  Give `serve` and `worker` different ports when they share a host.
- **OTLP (push):** set `OTEL_METRICS_EXPORTER=otlp` (or `console`, for stdout), or an endpoint in
  `OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`. The standard `OTEL_*` variables configure
  the exporter (`OTEL_EXPORTER_OTLP_PROTOCOL`, `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_METRIC_EXPORT_INTERVAL`,
  `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES`). `OTEL_METRICS_EXPORTER=none` turns it off; `prometheus` is
  rejected in favor of `-metrics-addr`.

On stop, each command exports its last metrics and spans, for up to 5 seconds. Metrics carry `service.name=durable-mcp` and
`durable_mcp.component=serve` or `worker`.

| Metric (Prometheus name) | Type | Attributes | Recorded by |
|---|---|---|---|
| `mcp.server.operation.duration` (`mcp_server_operation_duration_seconds`) | histogram, s | `mcp.method.name`, `gen_ai.tool.name`, `error.type` (`tool_error` or the JSON-RPC code) | serve |
| `durable_mcp.jobs.started` (`durable_mcp_jobs_started_total`) | counter | none | serve |
| `durable_mcp.jobs.finished` (`durable_mcp_jobs_finished_total`) | counter | `durable_mcp.job.outcome` (`completed`, `failed`, `canceled`), `error.type` | worker (workflow) |
| `durable_mcp.activity.attempts` (`durable_mcp_activity_attempts_total`) | counter | `durable_mcp.activity.name`, `durable_mcp.activity.retry`, `error.type` | worker |
| `durable_mcp.github.rate_limit.wait.duration` (`durable_mcp_github_rate_limit_wait_duration_seconds`) | histogram, s | `durable_mcp.github.rate_limit.kind` (`primary`, `secondary`) | worker |
| `http.server.request.duration` (`http_server_request_duration_seconds`) | histogram, s | otelhttp's: `http.request.method`, `http.response.status_code`, `url.scheme`, without `server.address` and `server.port`, which come from the client's `Host` header; 401s included | serve |
| `http.client.request.duration` (`http_client_request_duration_seconds`) | histogram, s | otelhttp's: `http.request.method`, `http.response.status_code`, `server.address` | worker (GitHub) |
| `temporal_*` | Temporal SDK | the SDK's own, such as `namespace`, `task_queue`, `workflow_type`, `activity_type` | both |

Every attribute value comes from a fixed set, so the number of time series does not grow with use: method and tool
names the server does not know become `_OTHER`, and no metric is labeled with a job ID, repository, or caller. Those
belong in logs and in Temporal. The workflow's metric skips replays, and is recorded after the workflow returns, so a
job is counted once. Activity attempts count the retries that Temporal keeps out of workflow history.

### Traces

Traces are exported over OTLP when `OTEL_TRACES_EXPORTER` is set (`otlp`, or `console` for stdout; `none` turns them
off) or an OTLP endpoint is (`OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`), so one
`OTEL_EXPORTER_OTLP_ENDPOINT` turns on metrics and traces together. The sampler is the SDK's (parent-based, always on);
`OTEL_TRACES_SAMPLER` changes it. A trace follows a job from the agent's request to GitHub:

```text
POST /mcp                                       serve, otelhttp (401s too, outside auth)
└─ tools/call start_report                      serve, one span per MCP request
   └─ StartWorkflow:RepositoryReport            serve, Temporal tracing interceptor
      └─ RunWorkflow:RepositoryReport           worker
         └─ StartActivity:report.FetchIssuesPage   worker
            └─ RunActivity:report.FetchIssuesPage  worker
               └─ GET                           worker, otelhttp on the GitHub client
```

Tool call spans follow the MCP semantic conventions (`mcp.method.name`, `gen_ai.tool.name`,
`gen_ai.operation.name=execute_tool`, `error.type`) with the metric's bounded values, and record no arguments,
results, or error messages. The server continues a W3C `traceparent` a client sends, but takes no baggage from it; the
GitHub client sends no trace context to GitHub.

No span records a credential. The instrumentation captures no headers, and every span then passes through a policy
before export: header attributes are dropped, `url.full` and `url.query` keep only the values of known GitHub query
parameters (`page`, `per_page`, `state`, …; any other value becomes `REDACTED`), and bearer and token-shaped strings
in any other text, such as an error message that quotes a URL, are redacted. String attributes are capped at 1 KiB
unless `OTEL_SPAN_ATTRIBUTE_VALUE_LENGTH_LIMIT` says otherwise. A test sends a canary token through an
authenticated request, a rejected one, and the GitHub client, and checks it is in no span.

### Grafana dashboard

[`deploy/grafana/durable-mcp.json`](deploy/grafana/durable-mcp.json) charts the metrics above: tool call latency
(p50, p95) and error rate by tool, requests to `/mcp` by status (401s are rejected tokens), jobs started and finished
by outcome, failed jobs by error type, GitHub requests by status and their duration, rate-limit waits, and activity
attempts, retries, and failed attempts.

To use it, run `serve` and `worker` with `-metrics-addr`, have Prometheus scrape both, and then in Grafana go to
**Dashboards → New → Import**, upload the file (or paste it), and pick the Prometheus data source. The `datasource`
variable at the top of the dashboard switches it to another one later. Metrics pushed over OTLP fit it too, if the backend
names them the way the Prometheus exporter does: dots become underscores, and the unit and `_total` suffixes are
added.

A test keeps the dashboard in step with the code: it runs a tool call through serve's HTTP handler, a rejected
token, and two reports through the Temporal test suite against a fake GitHub API, scrapes the same Prometheus
exporter `-metrics-addr` serves, and fails if any query names a metric or label that is not in the scrape.

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
- A dashboard test runs those paths into a Prometheus registry and checks that every metric and label the
  [Grafana dashboard](deploy/grafana/durable-mcp.json) queries is emitted.

## How this was built

This project is built with AI coding agents (Claude Code) under my direction: I set the goals, architecture, and
priorities, and the agents write and test the code.

## License

[MIT](LICENSE)
