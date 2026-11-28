# Runbook

What to check and what to do when durable-mcp misbehaves in operation. Each section starts with the symptom, then
the checks, then the actions. [architecture.md](architecture.md) explains how the parts fit, and the
[README](../README.md) has every flag, metric, and timeout.

The `temporal` commands below use the [Temporal CLI](https://docs.temporal.io/cli). Like durable-mcp, it reads
`TEMPORAL_ADDRESS` and `TEMPORAL_NAMESPACE` from the environment, or takes `--address` and `--namespace`. A job ID
is a workflow ID, so every `--workflow-id` (`-w`) below takes a job ID such as `report-6gmqnxnfeb3bdg27poofcuoe4i`.
The task queue is `durable-mcp` unless `-task-queue` (env `DURABLE_MCP_TASK_QUEUE`) says otherwise.

## Stuck or slow jobs

**Symptom.** An agent reports that a job has been `running` for a long time, or `job_status` says "Progress is
unavailable because no worker answered".

A job is not necessarily stuck because it is slow. A large report on a worker without a GitHub token can take about
ten hours, since GitHub allows such a worker 60 requests an hour. Start with the pending activity:

```sh
temporal workflow describe --workflow-id report-...
```

The **Pending Activities** section tells most cases apart.

**Waiting out a rate limit (healthy).** The activity is `Scheduled` with a `ScheduledTime` in the future, and
`LastFailure` has type `report.RateLimited`, a `nextRetryDelay`, and the benign category. From a real run against a
rate-limited API:

```text
Pending Activities: 1

  ActivityId            5
  Type                  report.FetchRepository
  State                 Scheduled
  Attempt               4
  MaximumAttempts       10
  ScheduledTime         9 minutes from now
  LastFailure           {"message":"github: GET /repos/o/r: 403 primary rate limit exceeded, retry at 2026-11-27T23:47:10Z","source":"GoSDK","applicationFailureInfo":{"type":"report.RateLimited","nextRetryDelay":"599.999961542s","category":"APPLICATION_ERROR_CATEGORY_BENIGN"}}
```

The Temporal server holds this wait. No worker slot is in use, and the job resumes at `ScheduledTime` on its own. The
worker logs `GitHub rate limit hit; retrying after the delay` at INFO, with `Kind` and `Delay`. **Action:** none,
unless the waits keep coming back (see [Rate-limit storms](#rate-limit-storms)). The retries are not workflow history
events, so `temporal workflow show` does not show them; the pending activity and the worker log do.

**Retrying another failure.** The activity is `Scheduled` with a near `ScheduledTime`, and `LastFailure` has type
`report.Unavailable` (GitHub 5xx or 408, a network error, a request timeout, or a body that does not decode). Temporal
retries on a backoff from 1 second to 1 minute, for at most 10 attempts. **Action:** check GitHub's status and the
**GitHub requests by status** panel; if GitHub is down, the job fails with `report.Unavailable` once its attempts run
out, and the agent can start a new report later.

**No worker polling.** For a job that has not run any activity yet, `describe` shows `Pending Activities: 0` and a
`HistoryLength` of 2 (the start and the first workflow task). For a job in progress, the activity stays `Started`
with a `LastHeartbeatTime` older than the 10-second heartbeat timeout, and then turns `Scheduled` with the next
attempt and a heartbeat timeout as its `LastFailure`. Confirm with the task queue:

```sh
temporal task-queue describe --task-queue durable-mcp
temporal task-queue describe --task-queue durable-mcp --task-queue-type activity
```

With no worker, **Pollers** is empty or its `LastAccessTime` values are old (the server drops a poller 5 minutes after
its last poll), and **Task Queue Statistics** shows `ApproximateBacklogCount` and `ApproximateBacklogAge` growing.
**Action:** start or fix the worker (its log says why it stopped: `Worker failed`, or
`connect to Temporal at ...`). Nothing is lost while it is down: the tasks wait in Temporal, and the jobs carry on
once a worker polls again. The 24-hour execution timeout keeps running, though, and a page's 2-hour schedule-to-close
budget does too.

**A busy worker.** Pollers are present, but the activity backlog grows and
`temporal_worker_task_slots_available{worker_type="ActivityWorker"}` is 0 on every worker. Each worker runs at most
4 activities at once. **Action:** see [Scaling workers](#scaling-workers).

To list the jobs that are open:

```sh
temporal workflow list --query 'WorkflowType = "RepositoryReport" AND ExecutionStatus = "Running"'
```

A job that is not worth waiting for can be canceled, as `cancel_job` would: `temporal workflow cancel --workflow-id
report-...`.

## Rate-limit storms

**Symptom.** The **Rate-limit waits** panel (GitHub row) climbs, the **GitHub requests by status** panel shows 403s or
429s, `job_status` answers "Waiting for GitHub rate limit until ...", and jobs that wait too long end in
**Failed jobs by error type** as `report.RateLimited`.

**Checks.**

- The **Rate-limit waits** panel splits by `durable_mcp_github_rate_limit_kind`. `primary` means the token's hourly
  budget is spent: the waits last until the reset, up to an hour. `secondary` means GitHub is pushing back on
  concurrency or request rate: the waits are shorter, but double with each attempt at a page. The panel's
  "average wait" series is in seconds on its own axis.
- The **Activity retries and failed attempts** panel shows `failed: report.RateLimited`. This project's
  `durable_mcp_activity_attempts_total` counts rate-limited attempts; the Temporal SDK's own failure metrics leave them
  out, because the error is marked benign.
- A worker without a token logs `GITHUB_TOKEN is not set` at startup, and gets 60 requests an hour.

**Actions.**

- Give the worker a token, or one with a higher limit, and restart it (see
  [Rotating the worker's GitHub token](#rotating-the-workers-github-token)). GitHub counts personal access tokens
  against the account's limit, so a second token from the same account does not add budget; a token for another
  account or a GitHub App can.
- Reduce the demand: ask agents for shorter windows or a lower `max_pages`, and cancel jobs nobody needs
  (`cancel_job`, or `temporal workflow cancel`).
- Let the waits run. They cost nothing on the worker, and they survive restarts.

**Do not raise worker concurrency to get through a storm.** While jobs wait, their slots are already free, so more
slots fetch nothing more. Worse, the pages of every waiting job retry together when the limit resets, and the cap of
4 concurrent activities per worker is what keeps that from becoming a burst of concurrent requests, which is what
GitHub's secondary rate limit punishes. Every rate-limited attempt also counts against a page's 10 attempts and its
2-hour budget, so more 403s fail jobs sooner.

## Scaling workers

- **One worker runs at most 4 activities at once**, which is 4 concurrent GitHub requests on its token
  (`maxConcurrentActivities` in [`cmd/durable-mcp/worker.go`](../cmd/durable-mcp/worker.go); it is not a flag). A
  report fetches its pages one at a time, so 4 is also the number of reports that can fetch at the same moment;
  any number of reports can be open, since a waiting report holds no slot.
- **More workers on the same task queue add capacity**, and Temporal spreads the tasks among them. But the cap is per
  worker, not per token: N workers that share one `GITHUB_TOKEN` make up to 4×N concurrent requests on it. Add
  workers when the activity backlog grows and slots are full while GitHub is not limiting you, and watch the
  `secondary` series of the **Rate-limit waits** panel afterwards. If it rises, the token is the bottleneck, and more
  workers on it will not help.
- **To cap the whole fleet's rate**, the Temporal SDK has `worker.Options.TaskQueueActivitiesPerSecond`: a limit the
  Temporal server enforces for the whole task queue, across all workers, unlike the per-worker
  `WorkerActivitiesPerSecond`. durable-mcp does not set it, and no flag exposes it, so using it means a code change in
  `serveWorker`. It limits activity starts per second, not concurrency, and setting it also turns off eager activity
  dispatch. In `temporal task-queue describe --task-queue durable-mcp --task-queue-type activity`, the pollers'
  `RatePerSecond` of 100000 is the SDK's default, meaning no limit.
- **Saturation shows** as `temporal_worker_task_slots_available{worker_type="ActivityWorker"}` at 0, and as a rising
  `ApproximateBacklogCount` in `temporal task-queue describe --task-queue durable-mcp --task-queue-type activity`.
  The dashboard has no panel for these; query them in Grafana's Explore view. `temporal_num_pollers` shows each
  worker's pollers by `poller_type`.
- Workflow tasks are cheap here (the workflow only adds up page summaries), so workflow capacity is rarely the limit.
- `serve` scales on its own: it keeps no state, so replicas need no session affinity.

## Rotating tokens

### Bearer tokens for `serve`

`serve` reads its tokens once, at startup, from `-auth-tokens-file` (env `DURABLE_MCP_AUTH_TOKENS_FILE`) or
`DURABLE_MCP_AUTH_TOKENS`.

1. Mint a new token for the same caller name: `durable-mcp token alice`. Give the `token:` line to the caller over
   a private channel; it is stored nowhere else.
2. **Replace** that caller's line in the tokens file with the new `entry:` line. Do not add a second line for the same
   name: names must be unique, and `serve` refuses to start with `caller "alice" is listed twice`. There is no way
   to accept the old and the new token for one name at once, and a second name would be a different owner that
   cannot see the first name's jobs.
3. Restart every `serve` replica (SIGTERM; requests in flight get up to 10 seconds). Until the caller switches to the
   new token, its requests get `401 Unauthorized`; the **HTTP requests to /mcp by status** panel shows them, and
   `serve` logs `Rejected an HTTP request`, at most once every 10 seconds.

The caller's jobs carry over, because ownership follows the name, not the token. To revoke a leaked token without
issuing a new one, delete its line and restart.

### Rotating the worker's GitHub token

The worker reads `GITHUB_TOKEN` at startup only. Restart every worker with the new token **before** revoking the old
one at GitHub: a page fetched with a revoked token fails with `report.Unauthorized`, which is not retried, so its
report fails. A stopping worker lets running activities finish for up to a minute (give it a 70-second grace period),
and anything left over is retried on another worker.

## Replay and nondeterminism failures

Temporal rebuilds a workflow by replaying its history through the current workflow code, so a worker running changed
workflow code must make the same decisions, in the same order, for runs that started before the change.

**What guards it.** `TestReplayRecordedHistory` in
[`internal/report/replay_test.go`](../internal/report/replay_test.go) replays two histories recorded from real runs,
one from before the activities took their `report.*` names and one after, against the current `RepositoryReport`.
A change that would make the workflow decide differently (a new or reordered activity, a changed loop) fails it with
a nondeterminism error. `make workflowcheck` statically checks the workflow code for nondeterministic calls. Neither
checks the histories of your own running jobs, and neither checks that a changed activity result still means the same
thing. To replay a real run before a deploy, export its history and add it next to the others:

```sh
temporal workflow show --workflow-id report-... --output json > internal/report/testdata/NAME.json
```

**Symptom after a deploy.** Jobs stop making progress, the worker logs `Failed to process workflow task.` with a
`[TMPRL1100]` error, and `temporal_workflow_task_execution_failed_total{failure_reason="NonDeterminismError"}` rises.
The worker uses the SDK's default panic policy, which blocks the workflow rather than failing it: the workflow task
is retried, and the run stays open.

**Actions.**

1. **Roll back every worker** to the previous version, all at once. Blocked runs then replay under the code that
   started them and carry on; nothing is lost.
2. Reproduce it: export an affected history as above, add it to `TestReplayRecordedHistory`, and see it fail.
3. Fix the change, usually with a `workflow.GetVersion` guard so that old runs keep the old path, then deploy again.
4. If a run cannot be saved, either reset it to its first workflow task, which runs it again from the start under
   the new code and fetches every page again, or terminate it and have the agent start a new report:

   ```sh
   temporal workflow reset --workflow-id report-... --type FirstWorkflowTask --reason "nondeterminism after deploy"
   temporal workflow terminate --workflow-id report-... --reason "nondeterminism after deploy"
   ```

## Deploys

**Deploy all workers at once: stop every old worker, then start the new ones.** Since the activities took their
`report.` names (`report.FetchIssuesPage` and the rest), a worker from before the rename does not know the names that
new runs schedule. In a mixed fleet, an old worker that picks up such a task fails the attempt with
`unable to find activityType=report.FetchIssuesPage`, and that attempt counts against the page's 10. Stopping all
workers loses nothing: tasks wait in Temporal, and the jobs carry on when the new workers poll.

**The old, unprefixed names stay registered for now.** Activities that were scheduled before the upgrade keep their
old names on the server, so the worker registers the activities under both names (see `Register` in
[`internal/report/workflow.go`](../internal/report/workflow.go)). The old names can go once no run started before the
rename is open, which is at most a job timeout (24 hours) after the last pre-rename worker stopped, and they are due
to be removed in an upcoming release. Before deploying a release without them, check that no such run is open:

```sh
temporal workflow count --query 'WorkflowType = "RepositoryReport" AND ExecutionStatus = "Running" AND StartTime < "YYYY-MM-DDTHH:MM:SSZ"'
```

with the time the last pre-rename worker stopped. A deployment still on a version from before the rename should
upgrade to a release that registers both names first, wait a day, and only then go further.

**Every deploy:**

- Give each container a stop grace period of at least 70 seconds (`docker stop -t 70`, `stop_grace_period: 70s`,
  `terminationGracePeriodSeconds: 70`), so a stopping worker gets its full minute.
- Keep the Temporal address, namespace, and task queue the same on `serve` and the worker.
- `serve` can be deployed separately and rolled replica by replica; it keeps no state. It starts even while Temporal
  is down, and its tools answer with an error that says Temporal is unreachable until it is back.
- A change to the workflow code needs the replay test to pass, and preferably a history from your own jobs in it
  (see [Replay and nondeterminism failures](#replay-and-nondeterminism-failures)).

## Health and readiness probes

Over HTTP, `serve` answers two endpoints on its MCP port, with no token:

| Endpoint | Answers | Use it for |
|---|---|---|
| `GET /healthz` | `200 ok` whenever the process can answer HTTP | Liveness. It checks nothing else, so a Temporal outage does not get `serve` restarted. |
| `GET /readyz` | `200 ok` if Temporal answers a health check within 2 seconds, else `503 unavailable` | Readiness. Give the probe a timeout above 2 seconds, such as `timeoutSeconds: 3`. |

```sh
curl -sS -i http://127.0.0.1:8080/healthz
curl -sS -i http://127.0.0.1:8080/readyz
```

The image has no shell or `curl`, so use HTTP probes (`httpGet` on Kubernetes), not exec probes. A `503` from
`/readyz` means `serve` cannot reach Temporal: check Temporal itself with
`temporal operator cluster health --address HOST:7233`, and `serve`'s log for
`Temporal is not reachable yet; tools will say so until it is` or `Temporal call failed`.

The worker has no health endpoint. Whether it is polling shows in `temporal task-queue describe --task-queue
durable-mcp` (its pollers and their `LastAccessTime`) and in `temporal_num_pollers` on its `/metrics`. A worker that
cannot reach Temporal at startup exits with an error. The stdio transport has no health endpoint either.

## Reading the dashboard

[`deploy/grafana/durable-mcp.json`](../deploy/grafana/durable-mcp.json) has four rows. Its `datasource` variable picks
the Prometheus data source.

**MCP tool calls** (from `serve`)

- **Tool call latency (p50, p95)**, by tool. Tools return within seconds, since the work runs in the background; a p95
  near the 30-second tool call deadline means Temporal is slow or unreachable.
- **Tool call error rate**, by tool: tool errors (`error_type="tool_error"`) and JSON-RPC errors. A rise across all
  tools usually means Temporal is unreachable; on one tool, read `serve`'s log. A failed job is not a tool error.
- **HTTP requests to /mcp by status.** A rise in 401s is a caller with a wrong or rotated token, or someone probing.

**Jobs**

- **Jobs started and finished**, finished by outcome (`completed`, `failed`, `canceled`). Started counts no idempotent
  retries. Finishes trail starts by a report's run time.
- **Failed jobs by error type.** `report.RateLimited`: see [Rate-limit storms](#rate-limit-storms).
  `report.Unauthorized`: fix the worker's `GITHUB_TOKEN`. `report.LinkRefused`: check `-github-api-url`.
  `report.NotFound`, `report.ValidationFailed`, `report.InvalidRequest`: the agents' requests; nothing to fix on
  the server. `report.Unavailable` and `timeout`: GitHub or the network.

**GitHub** (from the worker)

- **GitHub requests by status.** 403 and 429 are mostly rate limits; 5xx is GitHub.
- **GitHub request duration (p50, p95).** The client gives up on a request after 30 seconds.
- **Rate-limit waits**, by kind: how often requests are rate-limited, and the average wait before the retry, on a
  second axis in seconds.

**Activities** (from the worker)

- **Activity attempts**, by activity, retries included. Temporal retries on the server, so workflow history shows only
  each activity's last attempt; this panel shows them all.
- **Activity retries and failed attempts.** Retries by activity, and failed attempts by error type. A rate-limit
  storm shows here as `failed: report.RateLimited`.

The panels do not cover the Temporal SDK's `temporal_*` metrics, such as task slots and pollers, which
[Scaling workers](#scaling-workers) uses. Per-job questions go to Temporal and the logs: no metric is labeled with a
job ID, repository, or caller.
