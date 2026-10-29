package testkit

import (
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// seedQueueRun writes one run directly: this test is about where the dashboard
// presents lifecycle states, not how a run reached them.
func SeedQueueRun(t *testing.T, runtime *app.Runtime, id, repository string, number int, state workflow.State, phase workflow.Phase, pr int, updated string) {
	t.Helper()
	if _, err := runtime.DB.Exec(`INSERT INTO runs(id,repository,issue_number,state,current_phase,review_round,pr_number,updated_at)
 VALUES(?,?,?,?,?,0,?,?)`, id, repository, number, state, phase, pr, updated); err != nil {
		t.Fatal(err)
	}
}

// queueGroupBody returns the table rows from one group header to the next.
func QueueGroupBody(t *testing.T, body, group string) string {
	t.Helper()
	start := strings.Index(body, `id="group-`+group+`"`)
	if start < 0 {
		t.Fatalf("queue has no group %q", group)
	}
	end := len(body)
	if offset := strings.Index(body[start+1:], `id="group-`); offset >= 0 {
		end = start + 1 + offset
	}
	return body[start:end]
}
