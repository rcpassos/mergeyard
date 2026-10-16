package harness_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/harness"
)

func codexLimitFixture(t *testing.T) (harness.PhaseArtifacts, time.Time) {
	t.Helper()
	data, err := os.ReadFile("../../docs/research/codex-m3/fixtures/usage-limit-first-request/events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 10, 16, 12, 23, 30, 0, time.UTC)
	return harness.PhaseArtifacts{Stdout: data, ExitCode: 1, StartedAt: started}, started.Add(6 * time.Second)
}

func TestCodexClassifiesCapturedLimit(t *testing.T) {
	zone, err := time.LoadLocation("Europe/Lisbon")
	if err != nil {
		t.Fatal(err)
	}
	previous := time.Local
	time.Local = zone
	t.Cleanup(func() { time.Local = previous })
	artifacts, observed := codexLimitFixture(t)
	adapter := harness.NewCodex(config.Codex{})
	got := adapter.ClassifyFailure(artifacts, observed)
	want := time.Date(2026, 10, 16, 12, 53, 0, 0, time.UTC)
	if got.Kind != harness.TemporaryLimit || !got.ResetAt.Equal(want) || got.Source != "turn.failed.error.message.reset_time" {
		t.Fatalf("captured limit = %+v, want reset %s", got, want)
	}
	if caps := adapter.Capabilities(); !caps.TemporaryLimitDetection || caps.CreditExhaustionDetection {
		t.Fatalf("capabilities = %+v", caps)
	}
	// Identity is available before the failed turn, including on the first request.
	prefix := artifacts.Stdout[:strings.IndexByte(string(artifacts.Stdout), '\n')+1]
	if id, err := adapter.DiscoverSession(prefix); err != nil || id != "01a144aa-e286-7420-b07e-1fdbe4dca033" {
		t.Fatalf("early identity = %q: %v", id, err)
	}
}

func TestCodexCapturedOrdinaryFailures(t *testing.T) {
	data, err := os.ReadFile("../../docs/research/codex-m2/evidence.json")
	if err != nil {
		t.Fatal(err)
	}
	var evidence struct {
		Cases []struct {
			Name string `json:"case"`
			Run  struct {
				Exit int `json:"exit_code"`
			} `json:"run"`
			Events []json.RawMessage `json:"selected_events"`
			Stderr string            `json:"stderr"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &evidence); err != nil {
		t.Fatal(err)
	}
	for _, c := range evidence.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var stream []byte
			for _, event := range c.Events {
				stream = append(stream, event...)
				stream = append(stream, '\n')
			}
			got := harness.NewCodex(config.Codex{}).ClassifyFailure(harness.PhaseArtifacts{Stdout: stream, Stderr: []byte(c.Stderr), ExitCode: c.Run.Exit}, time.Now())
			if got.Kind != harness.Ordinary {
				t.Fatalf("negative control = %+v", got)
			}
		})
	}
}

func TestCodexUnknownLimitSignalsRemainOrdinary(t *testing.T) {
	artifacts, observed := codexLimitFixture(t)
	adapter := harness.NewCodex(config.Codex{})
	for _, tc := range []struct{ name, old, replacement string }{
		{"ASCII apostrophe", "You’ve", "You've"},
		{"different plan", "Upgrade to Pro", "Upgrade to Team"},
		{"cross day", "1:52 PM", "Oct 17th, 2026 1:52 PM"},
		{"no reset variant", "try again at 1:52 PM", "try again later"},
		{"other error", "You’ve hit your usage limit.", "HTTP 429."},
		{"tool text", `"type":"error"`, `"type":"item.completed"`},
		{"missing turn start", `{"type":"turn.started"}`, ``},
		{"duplicate identity", `{"type":"turn.started"}`, `{"type":"thread.started","thread_id":"7f9e8221-470e-43e0-adc7-41798c5c193e"}`},
		{"no failed turn", `"type":"turn.failed"`, `"type":"turn.completed"`},
		{"conflicting failure", `"error":{"message":"You’ve`, `"error":{"message":"Other: You’ve`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			modified := artifacts
			modified.Stdout = []byte(strings.ReplaceAll(string(artifacts.Stdout), tc.old, tc.replacement))
			if got := adapter.ClassifyFailure(modified, observed); got.Kind != harness.Ordinary {
				t.Fatalf("unknown variant = %+v", got)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		stdout []byte
		exit   int
	}{
		{"successful exit", artifacts.Stdout, 0},
		{"killed", artifacts.Stdout, 143},
		{"stderr only", nil, 1},
		{"malformed tail", append(append([]byte(nil), artifacts.Stdout...), []byte("{broken")...), 1},
		{"truncated", artifacts.Stdout[:len(artifacts.Stdout)-10], 1},
		{"later completion", append(append([]byte(nil), artifacts.Stdout...), []byte("{\"type\":\"turn.completed\"}\n")...), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := adapter.ClassifyFailure(harness.PhaseArtifacts{Stdout: tc.stdout, Stderr: artifacts.Stdout, ExitCode: tc.exit}, observed); got.Kind != harness.Ordinary {
				t.Fatalf("unknown output = %+v", got)
			}
		})
	}
}

func TestCodexUnreliableResetUsesCoreCooldown(t *testing.T) {
	previous := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = previous })
	artifacts, _ := codexLimitFixture(t)
	for _, tc := range []struct {
		name, clock string
		observed    time.Time
	}{
		{"stale", "1:52 PM", time.Date(2026, 10, 16, 14, 0, 0, 0, time.UTC)},
		{"previous day clock", "1:52 PM", time.Date(2026, 10, 16, 23, 59, 0, 0, time.UTC)},
		{"malformed clock", "25:52 PM", time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			modified := artifacts
			modified.Stdout = []byte(strings.ReplaceAll(string(artifacts.Stdout), "1:52 PM", tc.clock))
			got := harness.NewCodex(config.Codex{}).ClassifyFailure(modified, tc.observed)
			if got.Kind != harness.TemporaryLimit || !got.ResetAt.IsZero() || got.Source != "turn.failed.error.message" {
				t.Fatalf("unreliable reset = %+v", got)
			}
		})
	}
}

func TestCodexReplayAfterMidnightUsesCooldown(t *testing.T) {
	zone, err := time.LoadLocation("Europe/Lisbon")
	if err != nil {
		t.Fatal(err)
	}
	previous := time.Local
	time.Local = zone
	t.Cleanup(func() { time.Local = previous })
	artifacts, _ := codexLimitFixture(t)
	observed := time.Date(2026, 10, 17, 0, 1, 0, 0, zone)
	got := harness.NewCodex(config.Codex{}).ClassifyFailure(artifacts, observed)
	if got.Kind != harness.TemporaryLimit || !got.ResetAt.IsZero() || got.Source != "turn.failed.error.message" {
		t.Fatalf("replayed reset must use cooldown: %+v", got)
	}
}

func TestCodexMissingCaptureDateUsesCooldown(t *testing.T) {
	artifacts, observed := codexLimitFixture(t)
	artifacts.StartedAt = time.Time{}
	got := harness.NewCodex(config.Codex{}).ClassifyFailure(artifacts, observed)
	if got.Kind != harness.TemporaryLimit || !got.ResetAt.IsZero() {
		t.Fatalf("undated capture = %+v", got)
	}
}
