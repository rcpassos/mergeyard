package scheduler_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func loginHomeWithFailingLogout(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".bash_profile"), []byte("export STARTUP_STATE=ready\nstartup_function() { printf startup-ready; }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".bash_logout"), []byte("/usr/bin/false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestSystemStepLoginStartupAndSharedCommandsIgnoreLogoutFailure(t *testing.T) {
	_, rt, api, _, cfg, r := localFlow(t, successfulScript)
	cfg.Repositories[0].Setup = []string{"[ \"$STARTUP_STATE\" = ready ]; setup_state=ready; startup_function > setup-ready", "[ \"$setup_state\" = ready ]"}
	cfg.Repositories[0].Checks = []string{"[ \"$STARTUP_STATE\" = ready ]; check_state=ready; startup_function", "[ \"$check_state\" = ready ]; printf checks-passed"}
	cfg.Repositories[0].CheckRetries = 0
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r, Env: map[string]string{"HOME": loginHomeWithFailingLogout(t)}})
	if err != nil {
		t.Fatal(err)
	}
	var run workflow.Run
	waitForWithin(t, 15*time.Second, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		run = runs[0]
		return run.Phase == workflow.Review || run.State == workflow.NeedsAttention
	})
	if run.Phase != workflow.Review || len(run.SystemSteps) != 2 {
		t.Fatalf("logout changed successful system commands: %+v", run)
	}
	for _, step := range run.SystemSteps {
		if step.ExitCode == nil || *step.ExitCode != 0 || step.Status != "succeeded" {
			t.Fatalf("successful system step=%+v", step)
		}
	}
}

func TestSystemStepExplicitFailureKeepsExitCodeDespiteLogout(t *testing.T) {
	for _, step := range []string{"setup", "checks"} {
		for _, failure := range []struct {
			name, command string
			code          int
		}{{"exit", "printf expected-failure; exit 7", 7}, {"pipeline", "/usr/bin/false | /usr/bin/true", 1}} {
			t.Run(step+"/"+failure.name, func(t *testing.T) {
				_, rt, api, _, cfg, r := localFlow(t, successfulScript)
				commands := []string{failure.command, "printf should-not-run"}
				if step == "setup" {
					cfg.Repositories[0].Setup = commands
				} else {
					cfg.Repositories[0].Checks = commands
				}
				cfg.Repositories[0].CheckRetries = 0
				s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r, Env: map[string]string{"HOME": loginHomeWithFailingLogout(t)}})
				if err != nil {
					t.Fatal(err)
				}
				run := finish(t, s, workflow.NeedsAttention, workflow.Implement)
				if len(run.SystemSteps) != 1 || run.SystemSteps[0].ExitCode == nil || *run.SystemSteps[0].ExitCode != failure.code {
					t.Fatalf("logout replaced configured failure exit: %+v", run.SystemSteps)
				}
			})
		}
	}
}
