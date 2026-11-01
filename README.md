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
- [ ] **Durable jobs:** the report as a Temporal workflow and activities (one page per activity, heartbeats, retry
      policies), a worker command, and workflow tests.
- [ ] **MCP server:** `start_report`, `job_status`, `job_result`, and `cancel_job` tools on a Temporal client, over
      stdio and streamable HTTP.
- [ ] **Hardening:** idempotent starts, timeouts and cancellation end to end, input validation, an error taxonomy
      agents can act on, OpenTelemetry metrics and traces, a Grafana dashboard, and bearer-token auth.
- [ ] **Docs and release:** architecture and design notes, an operations runbook, a Dockerfile, and v0.1.0.

## Layout

```
cmd/durable-mcp/   command entry point (prints its version for now)
internal/github/   GitHub REST API client
```

The design notes for the GitHub client, including why it returns rate-limit errors instead of sleeping, are in its
[package documentation](internal/github/doc.go).

## Running the tests

You need Go 1.26 or newer. With the default `GOTOOLCHAIN=auto`, an older `go` command downloads a suitable toolchain.

```sh
make test   # go test -race ./...
make lint   # golangci-lint, at the version CI uses
make vuln   # govulncheck
make        # vet, test, and lint
```

The tests run against local `httptest` servers and never call the real GitHub API, so no token is needed.

## How this was built

This project is built with AI coding agents (Claude Code) under my direction: I set the goals, architecture, and
priorities, and the agents write and test the code.

## License

[MIT](LICENSE)
