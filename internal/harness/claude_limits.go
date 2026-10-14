package harness

import (
	"bytes"
	"encoding/json"
	"io"
	"time"
)

// This contract is limited to #61's headless weekly-rejected capture. In
// particular, HTTP 429 and unavailable overage credits alone are not limits.
func (*Claude) ClassifyFailure(artifacts PhaseArtifacts, observedAt time.Time) FailureClassification {
	ordinary := FailureClassification{Kind: Ordinary}
	if artifacts.ExitCode != 1 {
		return ordinary
	}
	decoder := json.NewDecoder(bytes.NewReader(artifacts.Stdout))
	var session string
	var limit *claudeWeeklyLimit
	var assistantLimited, completed bool
	for {
		var event struct {
			Type           string          `json:"type"`
			Subtype        string          `json:"subtype"`
			SessionID      string          `json:"session_id"`
			Error          string          `json:"error"`
			Info           json.RawMessage `json:"rate_limit_info"`
			IsError        *bool           `json:"is_error"`
			TerminalReason string          `json:"terminal_reason"`
			APIErrorStatus int             `json:"api_error_status"`
		}
		if err := decoder.Decode(&event); err == io.EOF {
			break
		} else if err != nil || event.Type == "" || completed {
			return ordinary
		}
		if session != "" && event.SessionID != session {
			return ordinary
		}
		switch event.Type {
		case "system":
			if event.Subtype == "init" {
				if session != "" || !sessionUUID.MatchString(event.SessionID) {
					return ordinary
				}
				session = event.SessionID
			}
		case "rate_limit_event":
			limit, assistantLimited = nil, false
			var info claudeWeeklyLimit
			if err := json.Unmarshal(event.Info, &info); err != nil {
				return ordinary
			}
			if session != "" && info.Status == "rejected" && info.Window == "seven_day" && info.UsingOverage != nil && !*info.UsingOverage {
				limit = &info
			}
		case "assistant":
			assistantLimited = limit != nil && event.Error == "rate_limit"
		case "result":
			if limit == nil || !assistantLimited || event.Subtype != "success" || event.IsError == nil || !*event.IsError || event.TerminalReason != "api_error" || event.APIErrorStatus != 429 {
				return ordinary
			}
			completed = true
		}
	}
	if !completed {
		return ordinary
	}
	outcome := FailureClassification{Kind: TemporaryLimit, Source: "rate_limit_info.status"}
	if reset := limit.resetAt(observedAt); !reset.IsZero() {
		outcome.ResetAt, outcome.Source = reset, "rate_limit_info.resetsAt"
	}
	return outcome
}

type claudeWeeklyLimit struct {
	Status       string          `json:"status"`
	Window       string          `json:"rateLimitType"`
	UsingOverage *bool           `json:"isUsingOverage"`
	Reset        json.RawMessage `json:"resetsAt"`
	Windows      json.RawMessage `json:"unifiedWindows"`
}

func (limit claudeWeeklyLimit) resetAt(observedAt time.Time) time.Time {
	// The observed top-level reset is Unix seconds for the rejected weekly
	// window, not the earlier five-hour reset or a date-less text diagnostic.
	var seconds int64
	if json.Unmarshal(limit.Reset, &seconds) != nil || seconds <= 0 || seconds > 253402300799 {
		return time.Time{}
	}
	var windows struct {
		Weekly struct {
			Reset json.RawMessage `json:"resetsAt"`
		} `json:"seven_day"`
	}
	if len(limit.Windows) > 0 && json.Unmarshal(limit.Windows, &windows) != nil {
		return time.Time{}
	}
	if len(windows.Weekly.Reset) > 0 {
		var weekly int64
		if json.Unmarshal(windows.Weekly.Reset, &weekly) != nil || weekly != seconds {
			return time.Time{}
		}
	}
	reset := time.Unix(seconds, 0).UTC()
	if !reset.After(observedAt) {
		return time.Time{}
	}
	return reset
}
