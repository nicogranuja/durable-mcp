package report_test

import (
	"testing"

	"go.temporal.io/sdk/worker"

	"github.com/nicogranuja/durable-mcp/internal/report"
)

// TestReplayRecordedHistory replays the histories of real reports against
// the current workflow code. Each history in testdata is from a run on a
// Temporal development server, started through the MCP server's
// start_report tool, over the default 30-day window. Only the worker's
// host name was replaced.
//
//   - repository_report_history.json is of temporalio/sdk-go, from before
//     the activities were renamed: the repository, two pages each of
//     issues and pull requests, and one of commits, under the unprefixed
//     activity names.
//   - repository_report_history_prefixed.json is of nicogranuja/durable-mcp,
//     under the report.* activity names, through serve with auth and
//     tracing on and a worker without a GitHub token: one page of each.
//
// A change to the workflow that would make it decide differently on
// replay, such as a new activity or a reordered step without a
// workflow.GetVersion guard, fails this test with a nondeterminism error.
// When the workflow changes on purpose, keep these histories, guarded by a
// version check, and record a new one alongside them.
func TestReplayRecordedHistory(t *testing.T) {
	t.Parallel()

	for _, file := range []string{
		"testdata/repository_report_history.json",
		"testdata/repository_report_history_prefixed.json",
	} {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			replayer := worker.NewWorkflowReplayer()
			replayer.RegisterWorkflow(report.RepositoryReport)
			if err := replayer.ReplayWorkflowHistoryFromJSONFile(nil, file); err != nil {
				t.Fatalf("replay: %v", err)
			}
		})
	}
}
