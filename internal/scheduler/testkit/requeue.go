package testkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/web"
)

// ghMarkedComments answers the gh calls of one publication whose comment is
// already on the pull request, so a requeue must not create a second one.
type GhMarkedComments struct {
	Mu       sync.Mutex
	Existing string
	Posts    int
}

func (g *GhMarkedComments) Run(_ context.Context, _ []byte, args ...string) ([]byte, []byte, error) {
	method, endpoint := GhCall(args)
	g.Mu.Lock()
	defer g.Mu.Unlock()
	switch {
	case method == "GET" && strings.HasSuffix(endpoint, "/comments"):
		if g.Existing == "" {
			return []byte("[]"), nil, nil
		}
		return []byte(`[{"id":9,"html_url":"https://github.com/owner/repo/pull/101#issuecomment-9","body":` + strconv.Quote(g.Existing) + `}]`), nil, nil
	case method == "POST" && strings.HasSuffix(endpoint, "/comments"):
		g.Posts++
		return nil, nil, errors.New("the comment was already delivered")
	}
	return nil, nil, fmt.Errorf("unexpected gh call: %v", args)
}

// seedStoppedReport inserts a stopped report publication for a run that
// already exists.
func SeedStoppedReport(t *testing.T, rt *app.Runtime, runID string, pr int) {
	t.Helper()
	ctx := context.Background()
	if _, err := rt.DB.ExecContext(ctx, `INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status,step) VALUES(?,?,'review','reviewer',1,1,'claude','succeeded','')`, runID+"-attempt", runID); err != nil {
		t.Fatal(err)
	}
	marker := fmt.Sprintf("<!-- mergeyard:report:%s -->", strings.ReplaceAll(runID, "-", ""))
	if _, err := rt.DB.ExecContext(ctx, `INSERT INTO report_publications(attempt_id,run_id,repository,pr_number,phase,round,attempt,body,state,stopped,failed,warning_code,warning)
		VALUES(?,?,'owner/repo',?,'review',1,1,?,'pending',1,1,'github.forbidden','Report is saved locally. PR comment publication cannot deliver this comment.')`,
		runID+"-attempt", runID, pr, marker+"\nReport"); err != nil {
		t.Fatal(err)
	}
}

// csrfToken reads the token the CLI API hands to a proven caller.
func CsrfToken(t *testing.T, rt *app.Runtime, server *web.Server) string {
	t.Helper()
	response := httptest.NewRecorder()
	server.ServeHTTP(response, ApiRequest(t, rt.Workspace.Root, http.MethodGet, "http://127.0.0.1:7331/api/status"))
	var status web.Status
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	return status.Token
}
