package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"

	"github.com/nicogranuja/durable-mcp/internal/auth"
	"github.com/nicogranuja/durable-mcp/internal/report"
)

// as returns ctx with the named caller, as the HTTP transport's auth sets
// it; the empty name means no caller, as over stdio.
func as(ctx context.Context, name string) context.Context {
	if name == "" {
		return ctx
	}
	return auth.WithCaller(ctx, auth.Caller{Name: name})
}

func TestJobIDsPerCaller(t *testing.T) {
	t.Parallel()

	const key = "c0ffee-1"
	ids := map[string]string{}
	for _, caller := range []string{"", "alice", "bob", "alice\x00x", "ali"} {
		id := jobIDForKey(caller, key)
		if err := validateJobID(id); err != nil {
			t.Errorf("jobIDForKey(%q, %q) = %q: %v", caller, key, id, err)
		}
		if other, ok := ids[id]; ok {
			t.Errorf("callers %q and %q get the same job ID %s for one key", other, caller, id)
		}
		ids[id] = caller
	}
	// Like the empty caller's IDs, caller-scoped IDs must never change.
	if got, want := jobIDForKey("alice", key), "report-ijkjp4acobqgpurvxr3hkp3nku"; got != want {
		t.Errorf("jobIDForKey(\"alice\", %q) = %q, want %q", key, got, want)
	}

	// Through the tool: two callers with the same key and arguments start
	// two jobs, each recording its owner.
	f := newFakeClient()
	tl := newTools(f)
	in := StartReportInput{Owner: "acme", Repo: "widgets", IdempotencyKey: key}
	_, a, errA := tl.startReport(as(t.Context(), "alice"), nil, in)
	_, b, errB := tl.startReport(as(t.Context(), "bob"), nil, in)
	if errA != nil || errB != nil {
		t.Fatalf("startReport() errors = %v, %v", errA, errB)
	}
	if !a.Created || !b.Created || a.JobID == b.JobID {
		t.Errorf("alice got %+v, bob got %+v; want two new jobs", a, b)
	}
	if a.JobID != jobIDForKey("alice", key) || b.JobID != jobIDForKey("bob", key) {
		t.Errorf("job IDs %s and %s, want the callers' key IDs", a.JobID, b.JobID)
	}
	for i, want := range []string{"alice", "bob"} {
		if got := f.starts[i].Memo[memoCaller]; got != want {
			t.Errorf("start %d: memo caller = %v, want %q", i, got, want)
		}
	}
	_, _, _ = tl.startReport(t.Context(), nil, in)
	if got := f.starts[2].Memo[memoCaller]; got != "" {
		t.Errorf("start without a caller: memo caller = %v, want \"\"", got)
	}
}

// TestJobsArePrivate checks that a caller cannot tell another caller's
// job from one that does not exist: every tool answers exactly as it does
// for an unknown job ID, and nothing is queried or canceled.
func TestJobsArePrivate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, owner, caller string
		// memo, if set, replaces the job's memo caller.
		memo any
	}{
		{name: "another caller", owner: "alice", caller: "bob"},
		{name: "no caller, as over stdio", owner: "alice", caller: ""},
		{name: "a caller, on a job started without one", owner: "", caller: "alice"},
		{name: "a caller whose name is a prefix", owner: "alice", caller: "ali"},
		{name: "a caller field that is not a string", owner: "alice", caller: "alice", memo: 7},
	}
	for _, tt := range tests {
		for _, st := range []enumspb.WorkflowExecutionStatus{
			enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED,
		} {
			t.Run(tt.name+"/"+runStatus(st), func(t *testing.T) {
				t.Parallel()
				f := newFakeClient()
				tl := newTools(f)
				_, started, err := tl.startReport(as(t.Context(), tt.owner), nil, StartReportInput{Owner: "acme", Repo: "widgets"})
				if err != nil {
					t.Fatal(err)
				}
				j := f.jobs[started.JobID]
				j.status = st
				j.result = report.Report{}
				if tt.memo != nil {
					j.memo[memoCaller] = tt.memo
				}

				// The answers for a job ID that names nothing.
				unknown := newTools(newFakeClient())
				ctx := as(t.Context(), tt.caller)
				in := JobInput{JobID: started.JobID}
				_, _, wantStatus := unknown.jobStatus(ctx, nil, in)
				_, _, wantResult := unknown.jobResult(ctx, nil, in)
				_, _, wantCancel := unknown.cancelJob(ctx, nil, in)

				_, statusOut, statusErr := tl.jobStatus(ctx, nil, in)
				_, resultOut, resultErr := tl.jobResult(ctx, nil, in)
				_, cancelOut, cancelErr := tl.cancelJob(ctx, nil, in)
				for _, c := range []struct {
					tool      string
					out       any
					got, want error
				}{
					{"job_status", statusOut, statusErr, wantStatus},
					{"job_result", resultOut, resultErr, wantResult},
					{"cancel_job", cancelOut, cancelErr, wantCancel},
				} {
					if c.got == nil || c.want == nil || c.got.Error() != c.want.Error() {
						t.Errorf("%s = %s, %v; want the unknown job's answer, %v", c.tool, mustJSON(c.out), c.got, c.want)
					}
				}
				if len(f.cancels) != 0 || f.queries != 0 || j.cancelRequested {
					t.Errorf("cancels %v, %d queries; want the job untouched", f.cancels, f.queries)
				}

				// The owner still sees and cancels it.
				if tt.memo == nil {
					ownCtx := as(t.Context(), tt.owner)
					if _, _, err := tl.jobStatus(ownCtx, nil, in); err != nil {
						t.Errorf("owner's job_status error = %v", err)
					}
					if _, _, err := tl.jobResult(ownCtx, nil, in); err != nil {
						t.Errorf("owner's job_result error = %v", err)
					}
					if _, _, err := tl.cancelJob(ownCtx, nil, in); err != nil {
						t.Errorf("owner's cancel_job error = %v", err)
					}
				}
			})
		}
	}
}

// TestJobsWithoutCallerField covers jobs started before callers existed:
// their memo has no caller field, and they belong to the empty caller.
func TestJobsWithoutCallerField(t *testing.T) {
	t.Parallel()

	f := newFakeClient()
	f.add(testJobID, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING).memo = map[string]any{memoFingerprint: "x"}
	tl := newTools(f)
	if _, _, err := tl.jobStatus(t.Context(), nil, JobInput{JobID: testJobID}); err != nil {
		t.Errorf("job_status without a caller = %v, want the job", err)
	}
	if _, _, err := tl.jobStatus(as(t.Context(), "alice"), nil, JobInput{JobID: testJobID}); err == nil ||
		err.Error() != notFoundError(testJobID).Error() {
		t.Errorf("job_status as alice = %v, want not found", err)
	}
}

// TestStartReportJobDeletedMeanwhile covers a start that Temporal refuses
// because the key's job exists, after which the job is gone: it had
// finished and was deleted when its retention period passed. Retrying
// then starts a new job, and the error says so.
func TestStartReportJobDeletedMeanwhile(t *testing.T) {
	t.Parallel()

	f := newFakeClient()
	f.startErr = serviceerror.NewWorkflowExecutionAlreadyStarted("Workflow execution already finished", "", "run-0")
	tl := newTools(f)
	in := StartReportInput{Owner: "acme", Repo: "widgets", IdempotencyKey: "deleted-key-1"}
	_, out, err := tl.startReport(as(t.Context(), "alice"), nil, in)
	if err == nil {
		t.Fatalf("startReport() = %+v, want an error", out)
	}
	for _, want := range []string{"was deleted", "Retry this call with the same arguments and idempotency_key", "starts a new job"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "Retrying will not help") {
		t.Errorf("error = %q, want no advice against retrying", err)
	}

	f.startErr = nil
	_, out, err = tl.startReport(as(t.Context(), "alice"), nil, in)
	if err != nil || !out.Created || out.JobID != jobIDForKey("alice", in.IdempotencyKey) {
		t.Errorf("retried startReport() = %+v, %v; want a new job with the key's ID", out, err)
	}
}

// TestStartReportKeyJobOfAnotherOwner covers a job at a caller's key ID
// with another owner in its memo, which only a changed memo or ID formula
// could cause. It is an internal error, not the deleted-job case, and the
// job is never returned.
func TestStartReportKeyJobOfAnotherOwner(t *testing.T) {
	t.Parallel()

	f := newFakeClient()
	tl := newTools(f)
	in := StartReportInput{Owner: "acme", Repo: "widgets", IdempotencyKey: "owned-key-1"}
	req, err := in.validate(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	j := f.add(jobIDForKey("alice", in.IdempotencyKey), enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING)
	j.memo = map[string]any{memoFingerprint: fingerprint(req), memoCaller: "mallory"}

	_, out, err := tl.startReport(as(t.Context(), "alice"), nil, in)
	if err == nil {
		t.Fatalf("startReport() = %+v, want an error", out)
	}
	for _, want := range []string{"internal error", "no job was started", "Use a new idempotency_key"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
	for _, leak := range []string{"mallory", "deleted", "already exists"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error = %q, want it not to contain %q", err, leak)
		}
	}
}

func TestStartReportDescription(t *testing.T) {
	t.Parallel()

	d := startReportTool.Description
	if strings.Contains(d, "shared by everyone") {
		t.Errorf("description says keys are shared:\n%s", d)
	}
	for _, want := range []string{"private to the caller", "another caller using the same key gets\nits own job"} {
		if !strings.Contains(d, want) {
			t.Errorf("description does not contain %q:\n%s", want, d)
		}
	}
}
