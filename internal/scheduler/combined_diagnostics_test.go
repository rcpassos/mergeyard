package scheduler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/web"
)

func TestStatusExposesLoadedEffectiveConfigurationAndSource(t *testing.T) {
	s, rt := fixture(t, &fakeGitHub{})
	cfg, _, err := config.Parse([]byte("concurrency: 4\nrepositories:\n  - repo: owner/repo\n    concurrency: 2\n    implementer:\n      agent: codex\n      model: implementation\n    reviewer:\n      model: independent-review\n"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "loaded.yaml")
	server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg, ConfigPath: path})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/api/status", nil))
	var status web.Status
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &status) != nil || status.Config == nil || status.ConfigPath != path || status.Workspace != rt.Workspace.Root || status.Config.Concurrency != 4 {
		t.Fatalf("effective configuration absent: %d %s", response.Code, response.Body.String())
	}
	repo := status.Config.Repositories[0]
	if repo.Concurrency != 2 || repo.Implementer.Agent != "codex" || repo.Implementer.Model != "implementation" || repo.Reviewer.Model != "independent-review" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("resolved roles or caps lost: %+v", repo)
	}
}
