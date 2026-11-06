package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nicogranuja/durable-mcp/internal/report"
)

// fakeClient is an in-memory Client. It keeps one run per workflow ID and
// answers the way the Temporal server does for the start options the tools
// use: a start with an ID that exists fails with
// WorkflowExecutionAlreadyStarted, whether the run is open or closed.
type fakeClient struct {
	mu   sync.Mutex
	jobs map[string]*fakeJob

	// Errors to return instead of answering, per method.
	startErr, describeErr, queryErr, getErr, cancelErr error

	// beforeCancel, if set, runs when CancelWorkflow is called, before it
	// answers, to change the run as if it finished concurrently.
	beforeCancel func(*fakeJob)

	starts  []client.StartWorkflowOptions
	queries int
	cancels []string // workflow ID and run ID, as "id/run"
}

// fakeJob is a workflow run.
type fakeJob struct {
	runID           string
	workflowType    string
	req             report.Request
	memo            map[string]any
	status          enumspb.WorkflowExecutionStatus
	cancelRequested bool
	startTime       time.Time
	closeTime       time.Time
	expiration      time.Time
	pending         []*workflowpb.PendingActivityInfo

	// progress answers the progress query; nil means no worker answers.
	progress *report.Progress
	// result and err are what the run returned.
	result report.Report
	err    error
}

func newFakeClient() *fakeClient {
	return &fakeClient{jobs: make(map[string]*fakeJob)}
}

// add records a run of the report workflow with id and returns it for the
// test to adjust.
func (f *fakeClient) add(id string, status enumspb.WorkflowExecutionStatus) *fakeJob {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := &fakeJob{
		runID:        fmt.Sprintf("run-%d", len(f.jobs)+1),
		workflowType: reportWorkflowType,
		status:       status,
		startTime:    time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC),
		progress:     &report.Progress{Phase: report.PhaseStarted},
	}
	f.jobs[id] = j
	return j
}

func (f *fakeClient) ExecuteWorkflow(ctx context.Context, opts client.StartWorkflowOptions, workflow any, args ...any) (client.WorkflowRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.starts = append(f.starts, opts)
	if f.startErr != nil {
		return nil, f.startErr
	}
	if reflect.ValueOf(workflow).Pointer() != reflect.ValueOf(report.RepositoryReport).Pointer() || len(args) != 1 {
		return nil, serviceerror.NewInvalidArgument("unexpected workflow or arguments")
	}
	if j, ok := f.jobs[opts.ID]; ok {
		return nil, serviceerror.NewWorkflowExecutionAlreadyStarted("Workflow execution is already running", "", j.runID)
	}
	j := &fakeJob{
		runID:        fmt.Sprintf("run-%d", len(f.jobs)+1),
		workflowType: reportWorkflowType,
		req:          args[0].(report.Request),
		memo:         opts.Memo,
		status:       enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING,
		startTime:    time.Now(),
		progress:     &report.Progress{Phase: report.PhaseStarted},
	}
	f.jobs[opts.ID] = j
	return fakeRun{f: f, id: opts.ID, runID: j.runID}, nil
}

func (f *fakeClient) DescribeWorkflowExecution(ctx context.Context, id, runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	j, err := f.job(id, runID)
	if err != nil {
		return nil, err
	}
	var memo *commonpb.Memo
	if len(j.memo) > 0 {
		memo = &commonpb.Memo{Fields: make(map[string]*commonpb.Payload)}
		for k, v := range j.memo {
			p, err := converter.GetDefaultDataConverter().ToPayload(v)
			if err != nil {
				return nil, err
			}
			memo.Fields[k] = p
		}
	}
	info := &workflowpb.WorkflowExecutionInfo{
		Execution: &commonpb.WorkflowExecution{WorkflowId: id, RunId: j.runID},
		Type:      &commonpb.WorkflowType{Name: j.workflowType},
		Status:    j.status,
		StartTime: timestamppb.New(j.startTime),
		Memo:      memo,
	}
	if !j.closeTime.IsZero() {
		info.CloseTime = timestamppb.New(j.closeTime)
	}
	ext := &workflowpb.WorkflowExecutionExtendedInfo{CancelRequested: j.cancelRequested}
	if !j.expiration.IsZero() {
		ext.ExecutionExpirationTime = timestamppb.New(j.expiration)
	}
	return &workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: info,
		WorkflowExtendedInfo:  ext,
		PendingActivities:     j.pending,
	}, nil
}

func (f *fakeClient) QueryWorkflow(ctx context.Context, id, runID, queryType string, _ ...any) (converter.EncodedValue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.queries++
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	j, err := f.job(id, runID)
	if err != nil {
		return nil, err
	}
	if queryType != report.ProgressQuery {
		return nil, serviceerror.NewQueryFailed("unknown queryType " + queryType)
	}
	if j.progress == nil {
		return nil, context.DeadlineExceeded // no worker answered
	}
	return encoded(*j.progress)
}

func (f *fakeClient) GetWorkflow(_ context.Context, id, runID string) client.WorkflowRun {
	return fakeRun{f: f, id: id, runID: runID}
}

func (f *fakeClient) CancelWorkflow(ctx context.Context, id, runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	f.cancels = append(f.cancels, id+"/"+runID)
	if f.cancelErr != nil {
		return f.cancelErr
	}
	j, err := f.job(id, runID)
	if err != nil {
		return err
	}
	if f.beforeCancel != nil {
		f.beforeCancel(j)
	}
	if j.status != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
		return serviceerror.NewNotFound("workflow execution already completed")
	}
	j.cancelRequested = true
	return nil
}

// job returns the run of id, which must be runID unless runID is empty.
// The caller holds f.mu.
func (f *fakeClient) job(id, runID string) (*fakeJob, error) {
	j, ok := f.jobs[id]
	if !ok || (runID != "" && runID != j.runID) {
		return nil, serviceerror.NewNotFound("workflow not found for ID: " + id)
	}
	return j, nil
}

// fakeRun is a handle on a fake workflow run.
type fakeRun struct {
	f         *fakeClient
	id, runID string
}

func (r fakeRun) GetID() string                  { return r.id }
func (r fakeRun) GetRunID() string               { return r.runID }
func (r fakeRun) GetFirstExecutionRunID() string { return r.runID }

func (r fakeRun) Get(ctx context.Context, valuePtr any) error {
	return r.GetWithOptions(ctx, valuePtr, client.WorkflowRunGetOptions{})
}

func (r fakeRun) GetWithOptions(ctx context.Context, valuePtr any, _ client.WorkflowRunGetOptions) error {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.f.getErr != nil {
		return r.f.getErr
	}
	j, err := r.f.job(r.id, r.runID)
	if err != nil {
		return err
	}
	if j.status == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
		return fmt.Errorf("fake: Get on a running workflow would block")
	}
	if j.err != nil {
		return j.err
	}
	v, err := encoded(j.result)
	if err != nil {
		return err
	}
	return v.Get(valuePtr)
}

// encoded returns v as Temporal encodes a query or workflow result.
func encoded(v any) (converter.EncodedValue, error) {
	p, err := converter.GetDefaultDataConverter().ToPayloads(v)
	if err != nil {
		return nil, err
	}
	return client.NewValue(p), nil
}

// mustJSON is v as indented JSON, for test failure messages.
func mustJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%#v", v)
	}
	return string(b)
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
