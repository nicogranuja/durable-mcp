package report_test

import (
	"testing"

	"go.temporal.io/sdk/worker"

	"github.com/nicogranuja/durable-mcp/internal/report"
)

// TestReplayRecordedHistory replays the history of a real report against
// the current workflow code. The history in testdata is from a run on a
// Temporal development server, started through the MCP server's
// start_report tool, of temporalio/sdk-go over the default 30-day window:
// the repository, two pages each of issues and pull requests, and one of
// commits. Only the worker's host name was replaced.
//
// A change to the workflow that would make it decide differently on
// replay, such as a new activity or a reordered step without a
// workflow.GetVersion guard, fails this test with a nondeterminism error.
// When the workflow changes on purpose, keep this history, guarded by a
// version check, and record a new one alongside it.
func TestReplayRecordedHistory(t *testing.T) {
	t.Parallel()

	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(report.RepositoryReport)
	if err := replayer.ReplayWorkflowHistoryFromJSONFile(nil, "testdata/repository_report_history.json"); err != nil {
		t.Fatalf("replay: %v", err)
	}
}
