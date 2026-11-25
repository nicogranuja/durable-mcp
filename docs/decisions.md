# Design decisions

Short records of the decisions that shape durable-mcp, each with its context, the decision, the alternatives
considered, and the consequences, the bad ones included. [architecture.md](architecture.md) shows how the parts fit.

## 1. One page per activity, returning compact aggregates

**Context.** A report on a large repository means hundreds of GitHub requests, spread over hours when rate limits
intervene. Temporal records every activity's input and result in workflow history, replays that history to rebuild a
workflow, and starts warning about a run's history long before it terminates it.

**Decision.** Each activity makes exactly one GitHub request: the repository, or one page of issues, pull requests, or
commits. The next page's cursor travels in one activity's result and the next one's input. An activity reduces its
page to a partial aggregate (counts, merge durations, commits per week and per author) and never returns raw items; the
workflow adds the partials up. `max_pages`, at most 200 per resource, bounds a report at about 3,600 events.

**Alternatives.** One activity per resource that pages through everything, heartbeating its position so a retry can
resume: fewer events, but a single long attempt, and a rate-limit wait would have to happen inside it. Returning raw
items to the workflow: simpler aggregation, but history would grow with the data, not with the number of pages.
Continue-as-new or child workflows to cut history: not needed while the page cap holds.

**Consequences.** A failure repeats one page, never the pages before it, and a crashed worker loses at most the page it
was fetching. History stays small and holds no issue text. The cost is a scheduling round trip per page, so a report is
slower than a tight loop would be, which is fine for a job nobody waits on. The median time to merge cannot be combined
from per-page medians, so pages carry one number per merged pull request. The cursor is a URL in history, which the
client checks against its base URL but does not sign. GitHub pages these listings by offset, so items that change
during a long report can shift between pages; the counts are a close estimate, not a snapshot.

## 2. Rate limits are typed errors with `NextRetryDelay`, not sleeps

**Context.** GitHub's primary rate limit can reset up to an hour away, and a worker without a GitHub token gets only 60
requests an hour. Something has to wait.

**Decision.** The GitHub client never sleeps or retries. It returns a `*RateLimitError` whose `RetryAt` says when a
retry can succeed. The activity turns it into a retryable `report.RateLimited` application error with `NextRetryDelay`
set from that time (at least a backoff that doubles per attempt, for secondary limits), and marks it benign. The
Temporal server holds the wait. `job_status` finds it in the pending activity and tells the agent "waiting for GitHub
rate limit until ...".

**Alternatives.** Sleeping in the client or the activity: it hides the wait, ties up a goroutine and an activity slot
for up to an hour, and needs heartbeats the whole time; a worker restart loses the wait. Catching the error in the
workflow and waiting on a durable timer: also durable, and the wait would be visible in history and the progress query,
but it adds workflow code and history events for every wait and duplicates what the retry policy already does.

**Consequences.** No worker capacity is spent waiting, and the wait survives restarts. Rate-limit retries count against
the activity's 10 attempts and its 2-hour schedule-to-close budget, so a limit that keeps coming back eventually fails
the job, with a type that tells the agent to try later or get the worker a better token. The wait is not in history or
the progress query, so `job_status` has to describe the workflow as well as query it. `NextRetryDelay` needs Temporal
Server 1.24.2 or later; an older server ignores it and retries on the policy's backoff, wasting requests.

## 3. The job ID is the workflow ID, and idempotency keys are hashed per caller

**Context.** Agents retry tool calls, and a retried `start_report` must not start a second report. The MCP server
should be restartable and replicable without losing track of jobs.

**Decision.** The job ID is the workflow ID, and `serve` stores nothing. Without an idempotency key the ID is random.
With one, it is a SHA-256 of the caller's name and the key, so the same caller's retry names the same workflow. The ID
reuse policy `RejectDuplicate` and the conflict policy `Fail` make a duplicate start an error, and `start_report` then
returns the existing job if the fingerprint in its memo matches the request, or an error if the key was reused for
different arguments.

**Alternatives.** A database that maps keys to workflow IDs: another stateful component to run, back up, and keep
consistent with Temporal. Using the key itself as the workflow ID: the key's text would show in the UI and logs, IDs
would have no fixed shape to validate, and keys would be shared across callers. Letting Temporal attach a duplicate
start to the running workflow: simpler, but it cannot tell a new job from an old one or check that a retry asks for the
same report.

**Consequences.** Temporal is the only source of truth, so `serve` can restart or run as several replicas. A key works
for the namespace's retention period; after that its job is deleted and the same key starts a new one (`start_report`
handles the job being deleted between the duplicate start and the lookup). The fingerprint is stored in memos, so its
format can never change. The empty caller (stdio, or HTTP without tokens) keeps the hash formula from before callers
existed, so a retry after that upgrade still finds its job.

## 4. Another caller's job looks exactly like a job that does not exist

**Context.** With bearer tokens, several callers share one server and one Temporal namespace. A caller must not read or
cancel another's jobs, and should not learn which job IDs exist.

**Decision.** `start_report` records the caller's name in the job's memo. Every tool that takes a job ID describes the
workflow and compares the memo's owner with the caller exactly ("" matches only ""), and returns the same not-found
error, byte for byte, for another caller's job, a workflow that is not a report, and an ID that does not exist. An
owner that cannot be decoded counts as someone else's. Combined with per-caller key hashing, a caller cannot even name
another's job by guessing its key.

**Alternatives.** A "forbidden" error: clearer to an honest caller, but it confirms the job exists. A namespace per
caller: strong isolation, but namespaces are an operator's unit, not something to create per token. A search attribute
with visibility queries: needs the attribute registered on the server, and visibility can lag behind a start.

**Consequences.** The check costs nothing extra, since each of those tools describes the job anyway. Ownership follows the
caller's name, so rotating a token keeps its jobs, and every valid token for a name sees that name's jobs. Jobs started
over stdio or without tokens belong to the empty caller and are invisible once tokens are on. The memo is readable by
anyone with access to the Temporal namespace; this isolates callers of the MCP server, not users of Temporal. The
responses are identical, but the timing is not engineered to be: a missing ID gets Temporal's not-found, while another
caller's job is described first. Job IDs carry 128 or more bits, so that difference does not help anyone find a job.

## 5. Loopback only unless callers authenticate, with only token hashes stored

**Context.** Anyone who can reach the HTTP transport can start and cancel jobs, which spend the worker's GitHub quota.
It should be safe by default, and usable from other machines or a container when configured to be.

**Decision.** Without tokens, `serve` listens only on a loopback IP literal (not `localhost`, which is only as loopback
as the resolver says) and refuses to start otherwise. Two checks keep browser pages out: `CrossOriginProtection`, and
the MCP SDK's DNS-rebinding `Host` check. With tokens, every request needs a bearer token, and any address is allowed,
with a warning when it is beyond loopback. Tokens are minted by `durable-mcp token NAME` (130 random bits), and the
server's configuration holds only `name:sha256` entries, compared in constant time against every entry. With tokens the
`Host` check is off, because a rebinding page has no token, and keeping it would break a same-host TLS proxy that keeps
the public `Host`.

**Alternatives.** OAuth, as the MCP authorization spec describes: right for a public multi-tenant server, but heavy for
a self-hosted one. A slow password hash such as bcrypt: unnecessary for long random tokens, and it would make every
request expensive. Allowing `0.0.0.0` with only a warning: easy to get wrong in a container.

**Consequences.** A leaked tokens file lets no one in, and the default run cannot be reached from outside the machine.
Rotating a token means editing the file and restarting `serve`; there are no expiries or scopes. The transport is plain
HTTP, so beyond one machine it needs a TLS-terminating proxy or a private network. Rejections are logged at most once
every 10 seconds, with a count, so a flood of bad tokens cannot flood the log.

## 6. Rename activities without `GetVersion`

**Context.** The activities and error types were first named `FetchIssuesPage`, `NotFound`, and so on. On a task queue
shared with other workers, such names can collide, so they became `report.FetchIssuesPage` and `report.NotFound`.
Changing what a workflow schedules normally needs a `workflow.GetVersion` guard so that runs in flight replay
deterministically.

**Decision.** No `GetVersion` guard. The Go SDK matches an activity in history to the workflow's command by the part of
the name after the last dot, so a run started under the old names replays under the new ones. The worker registers the
activities under both names, because activities scheduled before the upgrade keep their old names on the server, and
sets `DisableRegistrationAliasing`, since the workflow schedules by name. `NormalizeErrorType` maps the old unprefixed
error types to the new ones, so `job_result` classifies old failures the same way. A replay test runs histories recorded
from real runs under both the old and the new names.

**Alternatives.** A `GetVersion` branch: the standard tool, but it stays in the workflow code until every old run is
gone, and it guards something replay already tolerates. Keeping the old names: no migration, but the collision risk
stays. A new task queue for the renamed activities: no overlap with old runs, but old workers would have to keep
polling the old queue until its runs finish, and `serve` would need the new queue's name.

**Consequences.** The workflow code has no version branches. The decision rests on an SDK behavior, which the replay
test pins; if the SDK changed it, that test would fail before a deploy did. Old workers do not know the new names, so
all workers must be upgraded together. The unprefixed registration can go once no run from before the rename is open,
at most a job timeout (a day) after the upgrade.

## 7. Telemetry off by default, spans redacted, and the dashboard tested against the code

**Context.** The project is meant to run on a laptop as well as in a cluster, and it handles credentials: bearer tokens
on the way in, a GitHub token on the way out. Instrumentation libraries record URLs and sometimes headers, and a
dashboard that queries a renamed metric fails silently.

**Decision.** Nothing is exported unless asked: OTLP when the standard `OTEL_*` variables say so, Prometheus only with
`-metrics-addr`, on a private registry, on loopback for a bare `:port`. Metric attributes come from fixed sets, so no
metric is labeled with a job ID, repository, or caller. Every span passes through a redaction policy before export
(header attributes dropped, unknown query values replaced with `REDACTED`, token-shaped text redacted, values capped at 1 KiB),
and a canary test checks that a token sent through every path is in no span. A test runs the real code paths through
the real Prometheus exporter and fails if the Grafana dashboard queries a metric or label that is not emitted.

**Alternatives.** Telemetry on by default with an OTLP endpoint on localhost: what autoexport does when
nothing is set, but a plain run would then make network calls. Trusting the instrumentation not to capture credentials: true today, but one option or
library change away from a leak. Reviewing the dashboard by hand: what fails first when a metric is renamed.

**Consequences.** Operators have to opt in to every signal. Redaction loses some debugging detail, such as unknown
query parameters' values. Per-job questions go to logs and Temporal, not metrics. The sync test checks names and
labels, not whether a panel's query means what its title says.

## 8. One distroless, non-root image, with the metrics port not exposed

**Context.** Both roles have to run in containers, and the container should be small, hard to misuse, and honest about
what it publishes.

**Decision.** One image for both roles, since `serve` and `worker` are subcommands of one static binary. It is built in
two stages, with `-trimpath` and the version and commit in the binary, onto distroless `static` with CA certificates
and no shell, running as user 65532 by number (so Kubernetes' `runAsNonRoot` can check it). It exposes only 8080, the
MCP port. `/metrics` has no authentication, so its port is not exposed, and `docker run -P` does not publish it. The
README asks for a stop grace period of at least 70 seconds, so a stopping worker gets its full minute.

**Alternatives.** An Alpine or Debian base: a shell for `docker exec` debugging, but more packages to patch and more for
an attacker to use. An image per role: smaller in theory, but the binary is the same. Exposing 9464 for convenience:
one `-P` away from publishing unauthenticated metrics.

**Consequences.** There is no shell in the container, so debugging means logs, metrics, and the Temporal UI. A CI job
builds the image and checks that it runs as non-root, refuses an unauthenticated `0.0.0.0`, authenticates with a
mounted tokens file, serves a scrape, and stops cleanly. The image is not published, the base images are pinned by tag
rather than digest, and CI builds it for one platform only.
