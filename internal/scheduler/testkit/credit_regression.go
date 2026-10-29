package testkit

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

type DiscoveryFailureCreditHarness struct {
	*CreditHarness
	Reject bool
}

func (h *DiscoveryFailureCreditHarness) DiscoverSession(data []byte) (string, error) {
	if h.Reject && bytes.Contains(data, []byte("probe-ready")) {
		return "", &fault.Error{Code: "phase.result_invalid", Message: "Probe session discovery rejected"}
	}
	return h.ClassifiedHarness.DiscoverSession(data)
}

type CreditLostPushGit struct {
	*managedgit.Manager
	LoseAck bool
}

func (g *CreditLostPushGit) PushFix(ctx context.Context, run managedgit.Run, previous, target string) error {
	if err := g.Manager.PushFix(ctx, run, previous, target); err != nil {
		return err
	}
	if g.LoseAck {
		return &fault.Error{Code: "git.push_failed", Message: "Push acknowledgement lost"}
	}
	return nil
}

func AssertCreditConflict(t *testing.T, root string, server http.Handler, id, code, message string) {
	t.Helper()
	response := httptest.NewRecorder()
	server.ServeHTTP(response, ApiRequest(t, root, http.MethodGet, "http://127.0.0.1:7331/api/status"))
	var status web.Status
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &status) != nil {
		t.Fatalf("status %d %s", response.Code, response.Body.String())
	}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7331/api/runs/"+id+"/retry", nil)
	request.Header.Set("Origin", "http://127.0.0.1:7331")
	request.Header.Set("X-CSRF-Token", status.Token)
	SignAPIRequest(t, root, request)
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != 409 || response.Header().Get("X-Mergeyard-Error-Code") != code || !strings.Contains(response.Body.String(), message) {
		t.Fatalf("credit conflict lost useful response: %d %s", response.Code, response.Body.String())
	}
}

func AddBusyAccountProbe(t *testing.T, rt *app.Runtime, base *FakeGitHub, remote string, cfg config.Config, r runner.Runner, agent string) *scheduler.Scheduler {
	t.Helper()
	account := cfg.Repositories[0]
	account.Repo = "owner/account"
	account.Implementer.Agent = agent
	account.Concurrency = 1
	cfg.Concurrency = 2
	cfg.Repositories[0].Concurrency = 1
	cfg.Repositories = append(cfg.Repositories, account)
	base.Issues[account.Repo] = []github.Issue{Ready(8)}
	GitCommand(t, filepath.Dir(remote), "config", "--file", TestGitConfig(t), "--add", "url."+remote+".insteadOf", "https://github.com/owner/account.git")
	executable := cfg.Agents.Claude.Executable
	adapter := harness.HarnessAdapter(harness.NewClaude(cfg.Agents.Claude))
	identity := ""
	if agent == "codex" {
		executable = cfg.Agents.Codex.Executable
		adapter = harness.NewCodex(cfg.Agents.Codex)
		identity = CodexIdentity + "\n"
	}
	script, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	script = []byte("#!/bin/sh\ncase \"$(git branch --show-current)\" in mergeyard/issue-8)\n" + identity + "exit 1;; esac\n" + strings.TrimPrefix(string(script), "#!/bin/sh\n"))
	if err := os.WriteFile(executable, script, 0700); err != nil {
		t.Fatal(err)
	}
	h := &CreditHarness{ClassifiedHarness: &ClassifiedHarness{HarnessAdapter: adapter, Outcome: harness.FailureClassification{Kind: harness.CreditsExhausted}}, Detection: true}
	s, err := TestScheduler(cfg, SchedulerResources(rt), scheduler.Dependencies{GitHub: BranchGitHub{base}, Runner: r, Harnesses: map[string]harness.HarnessAdapter{agent: h}})
	if err != nil {
		t.Fatal(err)
	}
	var source workflow.Run
	WaitFor(t, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, run := range runs {
			if run.Repository == account.Repo {
				source = run
				return source.State == workflow.NeedsAttention
			}
		}
		return false
	})
	if _, err := s.Retry(context.Background(), source.ID); err != nil {
		t.Fatal(err)
	}
	return s
}
