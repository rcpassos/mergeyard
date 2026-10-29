package testkit

import (
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
)

// limitedHarness records an account-wide usage limit with a reported reset.
func LimitedHarness(t *testing.T, runtime *app.Runtime, agent string, reset time.Time) {
	t.Helper()
	if _, err := runtime.DB.Exec("INSERT INTO harness_limits(harness_type,limited_until,reset_time_source,detected_at,phase_attempt_id,signal_source,notified_until) VALUES(?,?,'reported',?,NULL,'fixture','')", agent, reset.Format(time.RFC3339Nano), time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
}
