package mcpserver

import (
	"fmt"
	"strings"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"

	"github.com/nicogranuja/durable-mcp/internal/report"
)

// newTools returns the tools on a fake client.
func newTools(f *fakeClient) *tools {
	return &tools{client: f, taskQueue: "test-queue", logger: discardLogger()}
}

func TestStartReportValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   StartReportInput
		want string
	}{
		{name: "missing owner", in: StartReportInput{Repo: "widgets"}, want: "owner and repo are required"},
		{name: "path in repo", in: StartReportInput{Owner: "acme", Repo: "widgets/../../user"}, want: "is not a valid GitHub repository"},
		{name: "unparsable since", in: StartReportInput{Owner: "acme", Repo: "widgets", Since: "last week"}, want: `since "last week" is not a date or time`},
		{name: "unparsable until", in: StartReportInput{Owner: "acme", Repo: "widgets", Until: "2026-13-01"}, want: `until "2026-13-01" is not a date or time`},
		{
			name: "since after until",
			in:   StartReportInput{Owner: "acme", Repo: "widgets", Since: "2026-10-31", Until: "2026-10-01"},
			want: "since (2026-10-31T00:00:00Z) must be before until (2026-10-01T00:00:00Z)",
		},
		{
			name: "window too long",
			in:   StartReportInput{Owner: "acme", Repo: "widgets", Since: "2024-01-01", Until: "2026-01-01"},
			want: "must not be longer than 366 days",
		},
		{
			name: "control character in key",
			in:   StartReportInput{Owner: "acme", Repo: "widgets", IdempotencyKey: "key\n"},
			want: "idempotency_key must be at most 200 characters of printable text",
		},
		{
			name: "key too long",
			in:   StartReportInput{Owner: "acme", Repo: "widgets", IdempotencyKey: strings.Repeat("k", 201)},
			want: "idempotency_key must be at most 200 characters",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeClient()
			_, out, err := newTools(f).startReport(t.Context(), nil, tt.in)
			if err == nil {
				t.Fatalf("startReport() = %+v, want an error", out)
			}
			for _, want := range []string{"invalid arguments: ", tt.want, "no job was started"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err, want)
				}
			}
			if len(f.starts) != 0 {
				t.Errorf("started %d workflows, want none", len(f.starts))
			}
		})
	}
}

func TestStartReportStartsJob(t *testing.T) {
	t.Parallel()

	f := newFakeClient()
	_, out, err := newTools(f).startReport(t.Context(), nil, StartReportInput{
		Owner: "acme", Repo: "widgets", Since: "2026-10-01", Until: "2026-10-15T12:00:00-07:00",
	})
	if err != nil {
		t.Fatalf("startReport() error = %v", err)
	}
	if !out.Created || out.Status != statusRunning || validateJobID(out.JobID) != nil {
		t.Errorf("startReport() = %+v, want a new running job with a valid ID", out)
	}
	if !strings.Contains(out.Message, "job_status") {
		t.Errorf("message = %q, want it to point to job_status", out.Message)
	}

	if len(f.starts) != 1 {
		t.Fatalf("started %d workflows, want 1", len(f.starts))
	}
	opts := f.starts[0]
	if opts.ID != out.JobID {
		t.Errorf("workflow ID = %q, want the job ID %q", opts.ID, out.JobID)
	}
	if opts.TaskQueue != "test-queue" {
		t.Errorf("task queue = %q, want test-queue", opts.TaskQueue)
	}
	if opts.WorkflowExecutionTimeout != JobTimeout {
		t.Errorf("execution timeout = %v, want %v", opts.WorkflowExecutionTimeout, JobTimeout)
	}
	if opts.WorkflowIDReusePolicy != enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE ||
		opts.WorkflowIDConflictPolicy != enumspb.WORKFLOW_ID_CONFLICT_POLICY_FAIL ||
		!opts.WorkflowExecutionErrorWhenAlreadyStarted {
		t.Errorf("ID policies = %v, %v, error when started %v; want RejectDuplicate, Fail, true",
			opts.WorkflowIDReusePolicy, opts.WorkflowIDConflictPolicy, opts.WorkflowExecutionErrorWhenAlreadyStarted)
	}

	want := report.Request{
		Owner: "acme", Repo: "widgets",
		Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2026, 10, 15, 19, 0, 0, 0, time.UTC),
	}
	got := f.jobs[out.JobID].req
	if got != want {
		t.Errorf("workflow request = %+v, want %+v", got, want)
	}
	if opts.Memo[memoFingerprint] != fingerprint(want) {
		t.Errorf("memo = %v, want the request fingerprint", opts.Memo)
	}
}

func TestStartReportWithoutKeyStartsNewJobs(t *testing.T) {
	t.Parallel()

	f := newFakeClient()
	tl := newTools(f)
	in := StartReportInput{Owner: "acme", Repo: "widgets"}
	_, first, err1 := tl.startReport(t.Context(), nil, in)
	_, second, err2 := tl.startReport(t.Context(), nil, in)
	if err1 != nil || err2 != nil {
		t.Fatalf("startReport() errors = %v, %v", err1, err2)
	}
	if first.JobID == second.JobID || !second.Created {
		t.Errorf("job IDs %q and %q, created %v: want two new jobs", first.JobID, second.JobID, second.Created)
	}
}

func TestStartReportIdempotencyKey(t *testing.T) {
	t.Parallel()

	f := newFakeClient()
	tl := newTools(f)
	in := StartReportInput{Owner: "acme", Repo: "widgets", Since: "2026-10-01", IdempotencyKey: "c0ffee-1"}

	_, first, err := tl.startReport(t.Context(), nil, in)
	if err != nil {
		t.Fatalf("first startReport() error = %v", err)
	}
	if !first.Created || first.JobID != jobIDForKey("c0ffee-1") {
		t.Fatalf("first startReport() = %+v, want a new job with the key's ID", first)
	}

	// A retry finds the same job, while it runs and after it finished,
	// whatever the case of the names, since GitHub ignores it.
	retry := in
	retry.Owner, retry.Since = "ACME", "2026-10-01T00:00:00Z"
	for _, st := range []enumspb.WorkflowExecutionStatus{
		enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED,
	} {
		f.jobs[first.JobID].status = st
		_, again, err := tl.startReport(t.Context(), nil, retry)
		if err != nil {
			t.Fatalf("retried startReport() error = %v", err)
		}
		if again.JobID != first.JobID || again.Created || again.Status != runStatus(st) {
			t.Errorf("retried startReport() = %+v, want job %s, not created, status %s", again, first.JobID, runStatus(st))
		}
		if !strings.Contains(again.Message, "already exists") {
			t.Errorf("message = %q, want it to say the job already exists", again.Message)
		}
	}
	if len(f.jobs) != 1 {
		t.Errorf("%d jobs exist, want 1", len(f.jobs))
	}

	// The same key for another request is an error, not the wrong job.
	other := in
	other.Repo = "gadgets"
	_, out, err := tl.startReport(t.Context(), nil, other)
	if err == nil || !strings.Contains(err.Error(), "already used to start a report with different arguments") {
		t.Errorf("startReport() with a reused key = %+v, %v; want an error about the key", out, err)
	}

	// Another key is another job.
	other.IdempotencyKey = "c0ffee-2"
	_, out, err = tl.startReport(t.Context(), nil, other)
	if err != nil || !out.Created || out.JobID == first.JobID {
		t.Errorf("startReport() with a new key = %+v, %v; want a new job", out, err)
	}
}

func TestStartReportTemporalErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "unavailable",
			err:  serviceerror.NewUnavailable("dial tcp 10.1.2.3:7233: connect: connection refused"),
			want: "did not respond in time or is unavailable, so the job may or may not have started",
		},
		{
			name: "unexpected",
			err:  serviceerror.NewInternal("database at db-7.internal:5432 is read-only"),
			want: "returned an unexpected error (code Internal). Retry once",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeClient()
			f.startErr = tt.err
			_, _, err := newTools(f).startReport(t.Context(), nil, StartReportInput{Owner: "acme", Repo: "widgets"})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("startReport() error = %v, want it to contain %q", err, tt.want)
			}
			for _, leak := range []string{"10.1.2.3", "db-7.internal"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("error = %q, want it not to contain the server's message", err)
				}
			}
		})
	}
}

func TestFingerprint(t *testing.T) {
	t.Parallel()

	req := report.Request{Owner: "acme", Repo: "widgets", Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	// Fingerprints are stored in running jobs' memos, so they must never
	// change.
	if got, want := fingerprint(req), "5dc004c46681a2608883e6bce8d954b1"; got != want {
		t.Errorf("fingerprint(%+v) = %q, want %q", req, got, want)
	}
	same := report.Request{Owner: "ACME", Repo: "Widgets", Since: req.Since.In(time.FixedZone("PDT", -7*3600))}
	if fingerprint(same) != fingerprint(req) {
		t.Errorf("fingerprint differs for %+v, the same request", same)
	}
	for _, other := range []report.Request{
		{Owner: "acme", Repo: "gadgets", Since: req.Since},
		{Owner: "acme", Repo: "widgets"},
		{Owner: "acme", Repo: "widgets", Since: req.Since, Until: req.Since.Add(time.Hour)},
		{Owner: "acme", Repo: "widgets", Since: req.Since, MaxPages: 5},
		{Owner: "acmew", Repo: "idgets", Since: req.Since},
	} {
		if fingerprint(other) == fingerprint(req) {
			t.Errorf("fingerprint(%+v) equals that of a different request", other)
		}
	}
}

func TestStartReportKeyOfForeignJob(t *testing.T) {
	t.Parallel()

	// A report workflow with the key's ID that start_report did not start,
	// so it has no fingerprint, is not returned as the caller's job.
	f := newFakeClient()
	f.add(jobIDForKey("k"), enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING)
	_, out, err := newTools(f).startReport(t.Context(), nil, StartReportInput{Owner: "acme", Repo: "widgets", IdempotencyKey: "k"})
	if err == nil || !strings.Contains(err.Error(), "already used to start a report with different arguments") {
		t.Errorf("startReport() = %+v, %v; want an error about the key", out, err)
	}
}

// TestStartReportTimeout covers a start that Temporal may have carried out
// even though it did not answer in time: the advice must not lead to a
// second job.
func TestStartReportTimeout(t *testing.T) {
	t.Parallel()

	t.Run("with a key", func(t *testing.T) {
		t.Parallel()
		f := newFakeClient()
		f.startErr = serviceerror.NewDeadlineExceeded("context deadline exceeded")
		_, _, err := newTools(f).startReport(t.Context(), nil, StartReportInput{Owner: "acme", Repo: "widgets", IdempotencyKey: "k-1"})
		if err == nil || !strings.Contains(err.Error(), "may or may not have started. Retry this call in a few seconds with the same arguments and idempotency_key") {
			t.Errorf("startReport() error = %v, want advice to retry with the same key", err)
		}
	})

	t.Run("without a key", func(t *testing.T) {
		t.Parallel()
		f := newFakeClient()
		f.startErr = serviceerror.NewUnavailable("connection reset")
		_, _, err := newTools(f).startReport(t.Context(), nil, StartReportInput{Owner: "acme", Repo: "widgets"})
		if len(f.starts) != 1 {
			t.Fatalf("started %d times, want 1", len(f.starts))
		}
		want := fmt.Sprintf("if it did, its job_id is %q. In a few seconds, call job_status with that job_id", f.starts[0].ID)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("startReport() error = %v, want it to contain %q", err, want)
		}
	})
}

func TestJobIDs(t *testing.T) {
	t.Parallel()

	if a, b := newJobID(), newJobID(); a == b {
		t.Errorf("newJobID() returned %q twice", a)
	}
	for _, id := range []string{newJobID(), jobIDForKey("x"), jobIDForKey(strings.Repeat("ü", 200))} {
		if err := validateJobID(id); err != nil {
			t.Errorf("validateJobID(%q) = %v", id, err)
		}
	}
	// The mapping must never change: a retry after an upgrade has to find
	// the job its key started before.
	if got, want := jobIDForKey("c0ffee-1"), "report-tu5qnk5a3ht6wirycj6t44ywpu"; got != want {
		t.Errorf("jobIDForKey(\"c0ffee-1\") = %q, want %q", got, want)
	}
	if jobIDForKey("a") == jobIDForKey("b") {
		t.Error("jobIDForKey maps two keys to one ID")
	}
	for _, id := range []string{
		"", "report-", "report-sdk-go", "workflow-" + strings.Repeat("a", 26), "report-" + strings.Repeat("A", 26),
		"report-" + strings.Repeat("a", 25) + "1", "report-" + strings.Repeat("a", 27),
	} {
		if err := validateJobID(id); err == nil {
			t.Errorf("validateJobID(%q) = nil, want an error", id)
		}
	}
}
