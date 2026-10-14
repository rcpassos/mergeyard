package harness_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/harness"
)

func claudeLimitFixture(t *testing.T, name string) (harness.PhaseArtifacts, time.Time) {
	t.Helper()
	dir := filepath.Join("../../docs/research/claude-m3/fixtures", name)
	read := func(name string) []byte {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	var run struct {
		ExitCode int       `json:"exit_code"`
		Started  time.Time `json:"started_at"`
	}
	if err := json.Unmarshal(read("run.json"), &run); err != nil {
		t.Fatal(err)
	}
	return harness.PhaseArtifacts{Stdout: read("events.jsonl"), Stderr: read("stderr.log"), ExitCode: run.ExitCode}, run.Started
}

func TestClaudeClassifiesCapturedWeeklyRejection(t *testing.T) {
	artifacts, observed := claudeLimitFixture(t, "weekly-rejected")
	adapter := harness.NewClaude(config.Claude{})
	outcome := adapter.ClassifyFailure(artifacts, observed)
	wantReset := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	if outcome.Kind != harness.TemporaryLimit || !outcome.ResetAt.Equal(wantReset) || outcome.Source != "rate_limit_info.resetsAt" {
		t.Fatalf("weekly rejection = %+v; want temporary limit until %s from rate_limit_info.resetsAt", outcome, wantReset)
	}
	if caps := adapter.Capabilities(); !caps.TemporaryLimitDetection || caps.CreditExhaustionDetection {
		t.Fatalf("observed weekly rejection requires temporary-limit detection only: %+v", caps)
	}
}

func TestClaudeLimitNegativeControls(t *testing.T) {
	adapter := harness.NewClaude(config.Claude{})
	for _, name := range []string{"allowed-warning", "authentication-failed", "weekly-resumed"} {
		t.Run(name, func(t *testing.T) {
			artifacts, observed := claudeLimitFixture(t, name)
			if got := adapter.ClassifyFailure(artifacts, observed); got.Kind != harness.Ordinary {
				t.Fatalf("negative control classified as %+v", got)
			}
			if name == "weekly-resumed" && !adapter.NativeSucceeded(artifacts) {
				t.Fatal("post-reset resume did not establish native success")
			}
		})
	}
}

func replaceClaudeLimitField(t *testing.T, data []byte, old, replacement string) []byte {
	t.Helper()
	if !strings.Contains(string(data), old) {
		t.Fatalf("fixture does not contain %s", old)
	}
	return []byte(strings.ReplaceAll(string(data), old, replacement))
}

func TestClaudeUnknownLimitSignalsRemainOrdinary(t *testing.T) {
	artifacts, observed := claudeLimitFixture(t, "weekly-rejected")
	adapter := harness.NewClaude(config.Claude{})
	for _, tc := range []struct {
		name, old, replacement string
	}{
		{"warning", `"status":"rejected"`, `"status":"allowed_warning"`},
		{"allowed", `"status":"rejected"`, `"status":"allowed"`},
		{"unknown window", `"rateLimitType":"seven_day"`, `"rateLimitType":"five_hour"`},
		{"overage", `"isUsingOverage":false`, `"isUsingOverage":true`},
		{"missing overage flag", `"isUsingOverage":false`, `"isUsingOverage":null`},
		{"missing assistant error", `"error":"rate_limit"`, `"error":null`},
		{"other assistant error", `"error":"rate_limit"`, `"error":"authentication_failed"`},
		{"normal result", `"is_error":true`, `"is_error":false`},
		{"missing result flag", `"is_error":true`, `"is_error":null`},
		{"no HTTP status", `"api_error_status":429`, `"api_error_status":null`},
		{"other HTTP status", `"api_error_status":429`, `"api_error_status":500`},
		{"turn bound", `"subtype":"success"`, `"subtype":"error_max_turns"`},
		{"unknown terminal reason", `"terminal_reason":"api_error"`, `"terminal_reason":"completed"`},
		{"no startup identity", `"subtype":"init"`, `"subtype":"other"`},
		{"different session", `"type":"assistant","session_id":"46f8963c-6b42-491e-9924-85fad092ecad"`, `"type":"assistant","session_id":"7f9e8221-470e-43e0-adc7-41798c5c193e"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			modified := artifacts
			modified.Stdout = replaceClaudeLimitField(t, artifacts.Stdout, tc.old, tc.replacement)
			if got := adapter.ClassifyFailure(modified, observed); got.Kind != harness.Ordinary {
				t.Fatalf("unobserved variant classified as %+v", got)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		stdout []byte
		exit   int
	}{
		{"zero exit", artifacts.Stdout, 0},
		{"cancelled process", artifacts.Stdout, 137},
		{"text diagnostic", []byte("You've hit your weekly limit · resets 11am (Europe/Lisbon)"), 1},
		{"HTTP status alone", []byte(`{"type":"result","is_error":true,"api_error_status":429}`), 1},
		{"interactive transcript", []byte(`{"type":"assistant","error":"rate_limit","isApiErrorMessage":true,"apiErrorStatus":429}`), 1},
		{"tool output", []byte(`{"type":"user","message":{"content":[{"type":"tool_result","content":` + string(mustMarshalString(t, string(artifacts.Stdout))) + `}]}}`), 1},
		{"truncated stream", artifacts.Stdout[:strings.LastIndex(strings.TrimSpace(string(artifacts.Stdout)), "\n")], 1},
		{"malformed tail", append(append([]byte(nil), artifacts.Stdout...), []byte("{broken")...), 1},
		{"later success", append(append([]byte(nil), artifacts.Stdout...), []byte(successEvent)...), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			modified := harness.PhaseArtifacts{Stdout: tc.stdout, Stderr: artifacts.Stdout, ExitCode: tc.exit}
			if got := adapter.ClassifyFailure(modified, observed); got.Kind != harness.Ordinary {
				t.Fatalf("unknown output classified as %+v", got)
			}
		})
	}
}

func mustMarshalString(t *testing.T, value string) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestClaudeUnreliableResetLeavesCooldownToCore(t *testing.T) {
	artifacts, observed := claudeLimitFixture(t, "weekly-rejected")
	adapter := harness.NewClaude(config.Claude{})
	for _, tc := range []struct{ name, old, replacement string }{
		{"missing", `"resetsAt":1791972000`, `"unusedReset":1791972000`},
		{"null", `"resetsAt":1791972000`, `"resetsAt":null`},
		{"string", `"resetsAt":1791972000`, `"resetsAt":"1791972000"`},
		{"fractional", `"resetsAt":1791972000`, `"resetsAt":1791972000.5`},
		{"milliseconds", `"resetsAt":1791972000`, `"resetsAt":1791972000000`},
		{"negative", `"resetsAt":1791972000`, `"resetsAt":-1`},
		{"overflow", `"resetsAt":1791972000`, `"resetsAt":9223372036854775808`},
		{"conflicting weekly reset", `"seven_day":{"utilization":1,"resetsAt":1791972000}`, `"seven_day":{"utilization":1,"resetsAt":1791896400}`},
		{"malformed reset windows", `"unifiedWindows":{"five_hour":{"utilization":0.65,"resetsAt":1791896400},"seven_day":{"utilization":1,"resetsAt":1791972000}}`, `"unifiedWindows":"unknown"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			modified := artifacts
			modified.Stdout = replaceClaudeLimitField(t, artifacts.Stdout, tc.old, tc.replacement)
			got := adapter.ClassifyFailure(modified, observed)
			if got.Kind != harness.TemporaryLimit || !got.ResetAt.IsZero() || got.Source != "rate_limit_info.status" {
				t.Fatalf("unreliable reset = %+v; want core cooldown", got)
			}
		})
	}
	for _, observed := range []time.Time{time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC), time.Date(2026, 10, 14, 11, 0, 0, 0, time.UTC)} {
		got := adapter.ClassifyFailure(artifacts, observed)
		if got.Kind != harness.TemporaryLimit || !got.ResetAt.IsZero() {
			t.Fatalf("expired reset at %s = %+v", observed, got)
		}
	}
}
