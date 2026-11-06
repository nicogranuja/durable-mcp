package mcpserver

import (
	"slices"
	"strings"
	"testing"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
)

func TestCancelJob(t *testing.T) {
	t.Parallel()

	f := newFakeClient()
	j := f.add(testJobID, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING)
	tl := newTools(f)

	_, out, err := tl.cancelJob(t.Context(), nil, JobInput{JobID: testJobID})
	if err != nil {
		t.Fatalf("cancelJob() error = %v", err)
	}
	if !out.CancelRequested || out.Status != statusRunning || !strings.HasPrefix(out.Message, "Cancellation requested;") {
		t.Errorf("cancelJob() = %s, want a cancellation request", mustJSON(out))
	}
	if want := []string{testJobID + "/" + j.runID}; !slices.Equal(f.cancels, want) {
		t.Errorf("canceled %v, want %v: the run that was described", f.cancels, want)
	}

	// Again: the describe shows the request, so nothing is sent.
	_, out, err = tl.cancelJob(t.Context(), nil, JobInput{JobID: testJobID})
	if err != nil {
		t.Fatalf("second cancelJob() error = %v", err)
	}
	if !out.CancelRequested || !strings.HasPrefix(out.Message, "Cancellation was already requested") {
		t.Errorf("second cancelJob() = %s, want it to report the earlier request", mustJSON(out))
	}
	if len(f.cancels) != 1 {
		t.Errorf("canceled %d times, want 1", len(f.cancels))
	}

	// Once the job ends, canceling it is a no-op that says how it ended.
	j.status = enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED
	_, out, err = tl.cancelJob(t.Context(), nil, JobInput{JobID: testJobID})
	if err != nil {
		t.Fatalf("third cancelJob() error = %v", err)
	}
	if out.CancelRequested || out.Status != statusCanceled || !strings.Contains(out.Message, "already finished as canceled") {
		t.Errorf("third cancelJob() = %s, want it to say the job already ended", mustJSON(out))
	}
}

func TestCancelJobFinished(t *testing.T) {
	t.Parallel()

	f := newFakeClient()
	f.add(testJobID, enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED)
	_, out, err := newTools(f).cancelJob(t.Context(), nil, JobInput{JobID: testJobID})
	if err != nil {
		t.Fatalf("cancelJob() error = %v", err)
	}
	if out.CancelRequested || out.Status != statusCompleted || !strings.Contains(out.Message, "nothing to cancel") {
		t.Errorf("cancelJob() = %s, want nothing canceled", mustJSON(out))
	}
	if len(f.cancels) != 0 {
		t.Errorf("canceled %v, want nothing sent", f.cancels)
	}
}

func TestCancelJobRaces(t *testing.T) {
	t.Parallel()

	t.Run("job finishes first", func(t *testing.T) {
		t.Parallel()
		f := newFakeClient()
		f.add(testJobID, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING)
		f.beforeCancel = func(j *fakeJob) { j.status = enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED }

		_, out, err := newTools(f).cancelJob(t.Context(), nil, JobInput{JobID: testJobID})
		if err != nil {
			t.Fatalf("cancelJob() error = %v", err)
		}
		if out.Status != statusCompleted || out.CancelRequested {
			t.Errorf("cancelJob() = %s, want it to report the job completed", mustJSON(out))
		}
	})

	t.Run("another cancel first", func(t *testing.T) {
		t.Parallel()
		f := newFakeClient()
		f.add(testJobID, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING)
		f.cancelErr = serviceerror.NewCancellationAlreadyRequested("cancellation already requested for this workflow")

		_, out, err := newTools(f).cancelJob(t.Context(), nil, JobInput{JobID: testJobID})
		if err != nil {
			t.Fatalf("cancelJob() error = %v", err)
		}
		if !out.CancelRequested || !strings.HasPrefix(out.Message, "Cancellation was already requested") {
			t.Errorf("cancelJob() = %s, want the earlier request reported", mustJSON(out))
		}
	})

	t.Run("Temporal unavailable", func(t *testing.T) {
		t.Parallel()
		f := newFakeClient()
		f.add(testJobID, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING)
		f.cancelErr = serviceerror.NewUnavailable("connection refused")

		_, _, err := newTools(f).cancelJob(t.Context(), nil, JobInput{JobID: testJobID})
		if err == nil || !strings.Contains(err.Error(), "cancel_job: the job service (Temporal) did not respond") {
			t.Errorf("cancelJob() error = %v, want a retryable tool error", err)
		}
	})
}
