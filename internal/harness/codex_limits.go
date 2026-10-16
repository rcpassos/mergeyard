package harness

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"time"
)

// Only #62's observed Plus-plan same-day rejection is bound. In particular,
// credit offers in this message do not establish exhausted credits, and neither
// tool text nor generic HTTP errors establish a temporary limit.
var codexLimitMessage = regexp.MustCompile(`^You’ve hit your usage limit\. Upgrade to Pro \(https://[^[:space:]()]+\), visit https://[^[:space:]()]+ to purchase more credits or try again at ([0-9]{1,2}:[0-9]{2} [AP]M)\.$`)

func (*Codex) ClassifyFailure(artifacts PhaseArtifacts, observedAt time.Time) FailureClassification {
	ordinary := FailureClassification{Kind: Ordinary}
	if artifacts.ExitCode != 1 {
		return ordinary
	}
	decoder := json.NewDecoder(bytes.NewReader(artifacts.Stdout))
	var message string
	var identity string
	var started, failed bool
	for {
		var event codexEvent
		if err := decoder.Decode(&event); err == io.EOF {
			break
		} else if err != nil || failed {
			return ordinary
		}
		switch event.Type {
		case "thread.started":
			if identity != "" || started || !sessionUUID.MatchString(event.ThreadID) {
				return ordinary
			}
			identity = event.ThreadID
		case "turn.started":
			if started {
				return ordinary
			}
			started = true
		case "error":
			if !started || message != "" || !codexLimitMessage.MatchString(event.Message) {
				return ordinary
			}
			message = event.Message
		case "turn.failed":
			if message == "" || event.Error.Message != message {
				return ordinary
			}
			failed = true
		default:
			// Mid-turn and other event shapes need their own captured evidence.
			// Fresh and resumed executions share this native event contract.
			return ordinary
		}
	}
	if !failed {
		return ordinary
	}
	outcome := FailureClassification{Kind: TemporaryLimit, Source: "turn.failed.error.message"}
	clock, err := time.Parse("3:04 PM", codexLimitMessage.FindStringSubmatch(message)[1])
	if err != nil {
		return outcome
	}
	// Local execution shares the CLI host's zone. The native text has neither
	// date nor zone: never roll a past clock time forward to an assumed next day.
	if artifacts.StartedAt.IsZero() || artifacts.StartedAt.After(observedAt) {
		return outcome
	}
	local := artifacts.StartedAt.In(time.Local)
	// If execution or offline reconciliation crosses a local date boundary,
	// this date-less message cannot establish which day its clock refers to.
	if local.Format("2006-01-02") != observedAt.In(time.Local).Format("2006-01-02") {
		return outcome
	}
	reset := time.Date(local.Year(), local.Month(), local.Day(), clock.Hour(), clock.Minute(), 0, 0, time.Local)
	// The captured message truncates seconds (12:52:00Z vs 12:52:10Z). Wait
	// through the displayed minute, and reject stale or DST-ambiguous clocks.
	stale := !reset.Add(time.Minute).After(observedAt)
	nonexistentClock := reset.Hour() != clock.Hour() || reset.Minute() != clock.Minute()
	repeatedClock := reset.Add(time.Hour).Format("15:04") == reset.Format("15:04") || reset.Add(-time.Hour).Format("15:04") == reset.Format("15:04")
	if stale || nonexistentClock || repeatedClock {
		return outcome
	}
	outcome.ResetAt, outcome.Source = reset.Add(time.Minute).UTC(), "turn.failed.error.message.reset_time"
	return outcome
}
