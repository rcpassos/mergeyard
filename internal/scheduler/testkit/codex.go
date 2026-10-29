package testkit

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

const CodexID = "01a10c5b-38a9-7330-b833-04e365cb5f37"
const CodexIdentity = `printf '%s\n' '{"type":"thread.started","thread_id":"01a10c5b-38a9-7330-b833-04e365cb5f37"}'`
const CodexResult = `last_message=''
previous=''
for arg do
 if [ "$previous" = '-o' ]; then last_message=$arg; fi
 previous=$arg
done
printf '%s\n' '{"schema_version":4,"verification":[],"criteria":[],"human_action":null,"status":"success","summary":"Added feature"}' > "$last_message"
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"schema_version\":4,\"verification\":[],\"criteria\":[],\"human_action\":null,\"status\":\"success\",\"summary\":\"Added feature\"}"}}' '{"type":"turn.completed"}'`
const CodexImplementation = CodexIdentity + "\n" + `printf '%s\n' implemented > feature.txt` + "\n" + CodexResult

func TmuxCodexFlow(t *testing.T, script string) (*scheduler.Scheduler, *app.Runtime, *FakeGitHub, string, config.Config, runner.Runner) {
	t.Helper()
	_, runtime, api, remote, cfg, r := TmuxFlow(t, SuccessfulScript)
	executable := filepath.Join(t.TempDir(), "fake-codex")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg.Agents.Codex.Executable = executable
	cfg.Repositories[0].Implementer = config.Role{Agent: "codex", Model: "chosen-model", Effort: "medium", Skills: []string{"implement", "check"}, MaxAttempts: 1}
	s, err := TestScheduler(cfg, SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	return s, runtime, api, remote, cfg, r
}

func WaitCodexIdentity(t *testing.T, s *scheduler.Scheduler) workflow.Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) == 1 && runs[0].Implementer != nil && runs[0].Implementer.SessionID == CodexID {
			return runs[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no early Codex identity in public run snapshot")
	return workflow.Run{}
}

type ObserveCodexLaunch struct {
	runner.Runner
	Observe func(runner.SessionRequest)
	Failure error
}

func (r ObserveCodexLaunch) StartSession(ctx context.Context, req runner.SessionRequest) (runner.SessionRef, error) {
	r.Observe(req)
	if r.Failure != nil {
		return runner.SessionRef{}, r.Failure
	}
	return r.Runner.StartSession(ctx, req)
}
